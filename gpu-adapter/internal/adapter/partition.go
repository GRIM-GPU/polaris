package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"go.uber.org/zap"
)

type PartitionRequest struct {
	Mode   string
	Delete []string
	Create []string
	Force  bool
}

func (a *Adapter) Partition(ctx context.Context, ref string, req PartitionRequest) (GPU, error) {
	if req.Mode != ModeMIG && len(req.Create)+len(req.Delete) > 0 {
		return GPU{}, &Error{Code: CodeInvalidSpec, Message: "create and delete are allowed only with mode MIG"}
	}
	g, err := a.GetGPU(ctx, ref)
	if err != nil {
		return GPU{}, err
	}
	unlock, err := a.lock(true, g.PciBusID)
	if err != nil {
		return GPU{}, err
	}
	defer unlock()
	if g, err = a.GetGPU(ctx, g.PciBusID); err != nil {
		return GPU{}, err
	}
	if g.Binding == "host" {
		if req.Mode == ModeMIG && len(g.MigProfiles) == 0 {
			return GPU{}, &Error{Code: CodeNotSupported, Message: fmt.Sprintf("GPU %s does not support MIG", g.PciBusID)}
		}
		if req.Mode == ModeTimeSlicing && !g.TimeSlicing {
			return GPU{}, &Error{Code: CodeNotSupported, Message: fmt.Sprintf("GPU %s does not support TIME_SLICING", g.PciBusID)}
		}
		for _, p := range req.Create {
			if !slices.ContainsFunc(g.MigProfiles, func(mp MigProfile) bool { return mp.Name == p }) {
				return GPU{}, &Error{Code: CodeInvalidSpec, Message: fmt.Sprintf("GPU %s has no MIG profile %s", g.PciBusID, p)}
			}
		}
	}
	for _, id := range req.Delete {
		if !slices.ContainsFunc(g.MigInstances, func(mi MigInstance) bool { return mi.InstanceID == id }) {
			return GPU{}, &Error{Code: CodeDeviceUnavailable, Message: fmt.Sprintf("GPU %s has no MIG instance %s", g.PciBusID, id)}
		}
	}
	if g.Mode == req.Mode && len(req.Create)+len(req.Delete) == 0 {
		return g, nil
	}

	migChange := g.Mode == ModeMIG || req.Mode == ModeMIG
	var removed, affected, scope []string
	lost := func(mode, key string) bool { return false }
	switch {
	case g.Mode == ModeMIG:
		removed = req.Delete
		if req.Mode != ModeMIG {
			removed = nil
			for _, mi := range g.MigInstances {
				removed = append(removed, mi.InstanceID)
			}
		}
		for _, id := range removed {
			affected = append(affected, g.migDevices[id])
		}
		scope = removed
		lost = func(mode, key string) bool { return mode == ModeMIG && slices.Contains(removed, key) }
	case req.Mode == ModeMIG:
		affected = []string{g.gpuDevice, g.vfioDevice}
		lost = func(mode, key string) bool { return key == g.uuid || key == g.PciBusID }
	case req.Mode == ModeFull:
		affected = []string{g.gpuDevice}
		lost = func(mode, key string) bool { return mode == ModeTimeSlicing && key == g.uuid }
	}
	affected = slices.DeleteFunc(affected, func(d string) bool { return d == "" })
	if g.Mode != ModeMIG && g.Binding == "host" {
		scope = []string{g.uuid}
	}

	refs, pods, err := a.users(ctx, affected)
	if err != nil {
		return GPU{}, err
	}
	if g.Binding == "vm" && (req.Mode == ModeTimeSlicing || len(refs) == 0) {
		return GPU{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("GPU %s is bound to vfio-pci; return it to the host before changing it to %s", g.PciBusID, req.Mode)}
	}
	processes := func() ([]int, error) {
		if len(scope) == 0 {
			return nil, nil
		}
		var procs []int
		err := a.session(func(s Session) error {
			for _, uuid := range scope {
				pids, err := s.ComputeProcesses(uuid)
				if err != nil {
					return err
				}
				procs = append(procs, pids...)
			}
			return nil
		})
		return procs, err
	}
	procs, err := processes()
	if err != nil {
		return GPU{}, err
	}
	if (len(refs) > 0 || len(procs) > 0) && !req.Force {
		return GPU{}, &Error{Code: CodeDeviceBusy, Message: fmt.Sprintf("workloads or processes are using GPU %s", g.PciBusID), Busy: &BusyDetail{Workloads: append([]WorkloadRef{}, refs...)}}
	}

	tainted := affected
	if migChange {
		tainted = slices.DeleteFunc(append([]string{g.gpuDevice, g.vfioDevice}, slices.Collect(maps.Values(g.migDevices))...), func(d string) bool { return d == "" })
	}
	dra := a.Vendor.DRA()
	rules := a.Kube.ResourceV1().DeviceTaintRules()
	defer func() {
		for _, d := range tainted {
			if err := rules.Delete(context.WithoutCancel(ctx), "gpu-adapter-"+a.Node+"-"+d, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				zap.L().Warn("cannot remove DeviceTaintRule", zap.String("device", d), zap.Error(err))
			}
		}
	}()
	for _, d := range tainted {
		_, err := rules.Create(ctx, &resourcev1.DeviceTaintRule{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-adapter-" + a.Node + "-" + d, Labels: map[string]string{LabelManaged: "gpu-adapter", labelNode: a.Node}},
			Spec: resourcev1.DeviceTaintRuleSpec{
				DeviceSelector: &resourcev1.DeviceTaintSelector{Driver: new(dra.Driver), Pool: &a.Node, Device: new(d)},
				Taint:          resourcev1.DeviceTaint{Key: PartitionTaint, Effect: resourcev1.DeviceTaintEffectNoSchedule},
			},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return GPU{}, err
		}
	}

	hit := func(typ string, o metav1.Object, pins []ContainerGPU) bool {
		return slices.Contains(refs, WorkloadRef{typ, o.GetNamespace(), o.GetName()}) ||
			slices.ContainsFunc(pins, func(p ContainerGPU) bool { return lost(p.Mode, p.GpuID+p.InstanceID) })
	}
	pinned := func(o metav1.Object) []ContainerGPU {
		var pins []ContainerGPU
		_ = json.Unmarshal([]byte(o.GetAnnotations()[annoGPUs]), &pins)
		return pins
	}
	deps, err := a.Kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return GPU{}, err
	}
	for _, d := range deps.Items {
		if (d.Spec.Replicas == nil || *d.Spec.Replicas > 0) && hit("container", &d, pinned(&d)) {
			d.Spec.Replicas = new(int32(0))
			setAnnotation(&d, annoReason, ReasonDeviceRemoved)
			if _, err := a.Kube.AppsV1().Deployments(d.Namespace).Update(ctx, &d, metav1.UpdateOptions{}); err != nil {
				return GPU{}, err
			}
		}
	}
	jobs, err := a.Kube.BatchV1().Jobs("").List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return GPU{}, err
	}
	for _, j := range jobs.Items {
		if (j.Spec.Suspend == nil || !*j.Spec.Suspend) && j.Status.CompletionTime == nil && j.Status.Failed == 0 && hit("container", &j, pinned(&j)) {
			j.Spec.Suspend = new(true)
			setAnnotation(&j, annoReason, ReasonDeviceRemoved)
			if _, err := a.Kube.BatchV1().Jobs(j.Namespace).Update(ctx, &j, metav1.UpdateOptions{}); err != nil {
				return GPU{}, err
			}
		}
	}
	vms, err := a.Dynamic.Resource(vmResource).Namespace("").List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return GPU{}, err
	}
	for _, vm := range vms.Items {
		var pcis []string
		_ = json.Unmarshal([]byte(vm.GetAnnotations()[annoGPUs]), &pcis)
		var pins []ContainerGPU
		for _, pci := range pcis {
			pins = append(pins, ContainerGPU{Mode: ModeFull, GpuID: pci})
		}
		if strategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy"); strategy != "Halted" && hit("vm", &vm, pins) {
			if err := a.updateVM(ctx, vm.GetNamespace(), vm.GetName(), "Halted", ReasonDeviceRemoved, nil); err != nil {
				return GPU{}, err
			}
		}
	}
	for _, p := range pods {
		err := a.Kube.PolicyV1().Evictions(p.Namespace).Evict(ctx, &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}})
		if err != nil && !apierrors.IsNotFound(err) {
			err = a.Kube.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return GPU{}, err
		}
	}
	if g.Binding == "vm" {
		return GPU{}, &Error{Code: CodeDeviceBusy, Message: fmt.Sprintf("the VM using GPU %s is stopped; retry after the GPU returns to the host driver", g.PciBusID)}
	}
	if req.Force {
		if procs, err = processes(); err != nil {
			return GPU{}, err
		}
		if (g.Mode == ModeMIG) != (req.Mode == ModeMIG) {
			holders, err := a.Host.Holders(g.minor)
			if err != nil {
				return GPU{}, err
			}
			procs = append(procs, holders...)
		}
		if len(procs) > 0 {
			if err := a.Host.Kill(procs); err != nil {
				return GPU{}, &Error{Code: CodeDeviceBusy, Message: fmt.Sprintf("cannot stop processes on GPU %s: %v", g.PciBusID, err)}
			}
		}
	}

	if migChange {
		if err := a.session(func(s Session) error {
			if req.Mode == ModeMIG {
				for _, id := range removed {
					if err := s.DeleteMigInstance(id); err != nil {
						return err
					}
				}
			}
			if (g.Mode == ModeMIG) != (req.Mode == ModeMIG) {
				if err := s.SetMigMode(g.PciBusID, req.Mode == ModeMIG); err != nil {
					return err
				}
			}
			for _, p := range req.Create {
				if err := s.CreateMigInstance(g.PciBusID, p); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return GPU{}, err
		}

		plugins, err := a.Kube.CoreV1().Pods(a.PluginNamespace).List(ctx, metav1.ListOptions{LabelSelector: a.PluginSelector, FieldSelector: "spec.nodeName=" + a.Node})
		if err != nil {
			return GPU{}, err
		}
		for _, p := range plugins.Items {
			if err := a.Kube.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return GPU{}, err
			}
		}
	}

	records := a.Kube.CoreV1().ConfigMaps(a.Namespace)
	if err := retry.OnError(retry.DefaultRetry, func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }, func() error {
		record, err := records.Get(ctx, "gpu-adapter-"+a.Node, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			record, err = records.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gpu-adapter-" + a.Node, Namespace: a.Namespace}}, metav1.CreateOptions{})
		}
		if err != nil {
			return err
		}
		var timeSlicing []string
		_ = json.Unmarshal([]byte(record.Data["timeSlicing"]), &timeSlicing)
		timeSlicing = slices.DeleteFunc(timeSlicing, func(pci string) bool { return pci == g.PciBusID })
		if req.Mode == ModeTimeSlicing {
			timeSlicing = append(timeSlicing, g.PciBusID)
		}
		raw, _ := json.Marshal(timeSlicing)
		record.Data = map[string]string{"timeSlicing": string(raw)}
		_, err = records.Update(ctx, record, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return GPU{}, err
	}
	return a.GetGPU(ctx, g.PciBusID)
}
