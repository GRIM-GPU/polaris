package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"go.uber.org/zap"
)

type VMCreate struct {
	Common
	GPUs     []string
	UserData string
	Volumes  []VMVolume
}

type VMVolume struct {
	Name string
}

type VM struct {
	Namespace string
	Name      string
	Status    Status
	GPUs      []string
	Endpoint  string
}

func (a *Adapter) vmClaim(ctx context.Context, pcis []string) (resourcev1.ResourceClaimSpec, func(failed bool), error) {
	var spec resourcev1.ResourceClaimSpec
	for i, pci := range pcis {
		if slices.Index(pcis, pci) != i {
			return spec, nil, &Error{Code: CodeInvalidSpec, Message: "gpus has duplicate items"}
		}
	}
	unlock, err := a.lock(true, pcis...)
	if err != nil {
		return spec, nil, err
	}
	var emptied []string
	release := func(failed bool) {
		if failed && len(emptied) > 0 {
			if err := a.session(func(s Session) error {
				var errs []error
				for _, pci := range emptied {
					errs = append(errs, s.SetPersistence(pci, true))
				}
				return errors.Join(errs...)
			}); err != nil {
				zap.L().Warn("cannot turn persistence mode back on", zap.Strings("gpus", emptied), zap.Error(err))
			}
		}
		unlock()
	}
	done := false
	defer func() {
		if !done {
			release(true)
		}
	}()
	nodeGPUs, err := a.ListGPUs(ctx)
	if err != nil {
		return spec, nil, err
	}
	gpus, err := vmGPUs(nodeGPUs, pcis)
	if err != nil {
		return spec, nil, err
	}
	dra := a.Vendor.DRA()
	var requests []string
	for i, g := range gpus {
		refs, _, err := a.users(ctx, slices.DeleteFunc(append([]string{g.gpuDevice, g.vfioDevice}, slices.Collect(maps.Values(g.migDevices))...), func(d string) bool { return d == "" }))
		if err != nil {
			return spec, nil, err
		}
		if len(refs) > 0 {
			return spec, nil, &Error{Code: CodeDeviceBusy, Message: fmt.Sprintf("workloads are using GPU %s", g.PciBusID), Busy: &BusyDetail{Workloads: append([]WorkloadRef{}, refs...)}}
		}
		spec.Devices.Requests = append(spec.Devices.Requests, resourcev1.DeviceRequest{Name: fmt.Sprintf("gpu%d", i), Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: dra.Classes[DeviceVFIO],
			Selectors:       []resourcev1.DeviceSelector{{CEL: &resourcev1.CELDeviceSelector{Expression: fmt.Sprintf(`device.attributes["resource.kubernetes.io"].pciBusID == %q`, g.PciBusID)}}},
		}})
		requests = append(requests, fmt.Sprintf("gpu%d", i))
	}
	spec.Devices.Config = []resourcev1.DeviceClaimConfiguration{{
		Requests:            requests,
		DeviceConfiguration: resourcev1.DeviceConfiguration{Opaque: &resourcev1.OpaqueDeviceConfiguration{Driver: dra.Driver, Parameters: dra.VFIO}},
	}}
	for _, g := range gpus {
		if g.Binding != "host" {
			continue
		}
		if err := a.session(func(s Session) error {
			if err := s.SetPersistence(g.PciBusID, false); err != nil {
				return err
			}
			if err := s.SetDrain(g.PciBusID, true); err != nil {
				return errors.Join(err, s.SetPersistence(g.PciBusID, true))
			}
			return nil
		}); err != nil {
			return spec, nil, err
		}
		emptied = append(emptied, g.PciBusID)
		holders, err := a.Host.Holders(g.minor)
		for try := 0; err == nil && len(holders) > 0 && try < 3; try++ {
			_ = a.Host.Kill(holders)
			holders, err = a.Host.Holders(g.minor)
		}
		if err == nil && len(holders) > 0 {
			err = &Error{Code: CodeDeviceBusy, Message: fmt.Sprintf("processes %v keep GPU %s open", holders, g.PciBusID)}
		}
		if err := errors.Join(err, a.session(func(s Session) error { return s.SetDrain(g.PciBusID, false) })); err != nil {
			return spec, nil, err
		}
	}
	done = true
	return spec, release, nil
}

func vmGPUs(nodeGPUs []GPU, pcis []string) ([]GPU, error) {
	var gpus []GPU
	for _, pci := range pcis {
		at := slices.IndexFunc(nodeGPUs, func(g GPU) bool { return g.PciBusID == pci })
		if at < 0 {
			return nil, &Error{Code: CodeDeviceUnavailable, Message: fmt.Sprintf("GPU %s does not exist", pci)}
		}
		g := nodeGPUs[at]
		if g.Mode != ModeFull {
			return nil, &Error{Code: CodeDeviceUnavailable, Message: fmt.Sprintf("GPU %s is in %s mode; a VM takes only FULL GPUs", pci, g.Mode)}
		}
		if !g.VmAttachable {
			return nil, &Error{Code: CodeNotSupported, Message: fmt.Sprintf("GPU %s shares its IOMMU group and cannot be assigned to a VM", pci)}
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

func vmDevices(n int) []any {
	var gpus []any
	for i := range n {
		gpus = append(gpus, map[string]any{"name": fmt.Sprintf("gpu%d", i), "claimName": "gpu", "requestName": fmt.Sprintf("gpu%d", i)})
	}
	return gpus
}

func (a *Adapter) CreateVM(ctx context.Context, req VMCreate) (VM, error) {
	if req.Image == "" {
		return VM{}, &Error{Code: CodeInvalidSpec, Message: "image is required"}
	}
	ns, name := req.Namespace, req.Name
	req.GPUs = lower(req.GPUs)
	disks := []any{map[string]any{"name": "root", "disk": map[string]any{"bus": "virtio"}}}
	volumes := []any{map[string]any{"name": "root", "containerDisk": map[string]any{"image": req.Image}}}
	var names []string
	for _, v := range req.Volumes {
		if v.Name == "" {
			return VM{}, &Error{Code: CodeInvalidSpec, Message: "volumes need a name"}
		}
		names = append(names, v.Name)
		disks = append(disks, map[string]any{"name": "vol-" + v.Name, "disk": map[string]any{"bus": "virtio"}})
		volumes = append(volumes, map[string]any{"name": "vol-" + v.Name, "persistentVolumeClaim": map[string]any{"claimName": v.Name}})
	}
	if req.UserData != "" {
		disks = append(disks, map[string]any{"name": "cloudinit", "disk": map[string]any{"bus": "virtio"}})
		volumes = append(volumes, map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": name + "-cloudinit"}}})
	}
	if err := a.prepare(ctx, ns, name, "vm", names); err != nil {
		return VM{}, err
	}

	spec, release, err := a.vmClaim(ctx, req.GPUs)
	if err != nil {
		return VM{}, err
	}
	created := false
	defer func() { release(!created) }()
	raw, _ := json.Marshal(req.GPUs)
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   map[string]any{"name": name, "namespace": ns, "labels": map[string]any{LabelManaged: "gpu-adapter", labelNode: a.Node}, "annotations": map[string]any{annoGPUs: string(raw)}},
		"spec": map[string]any{
			"runStrategy": "Always",
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{labelWorkload: name}},
				"spec": map[string]any{
					"resourceClaims": []any{map[string]any{"name": "gpu", "resourceClaimName": name + "-gpu"}},
					"domain": map[string]any{
						"cpu":       map[string]any{"cores": (req.CPUMillicores + 999) / 1000},
						"memory":    map[string]any{"guest": fmt.Sprint(req.MemoryBytes)},
						"resources": map[string]any{"requests": map[string]any{"cpu": fmt.Sprintf("%dm", req.CPUMillicores), "memory": fmt.Sprint(req.MemoryBytes)}},
						"devices": map[string]any{
							"interfaces": []any{map[string]any{"name": "default", "masquerade": map[string]any{}}},
							"gpus":       vmDevices(len(req.GPUs)),
							"disks":      disks,
						},
					},
					"networks": []any{map[string]any{"name": "default", "pod": map[string]any{}}},
					"volumes":  volumes,
				},
			},
		},
	}}
	vm, err = a.Dynamic.Resource(vmResource).Namespace(ns).Create(ctx, vm, metav1.CreateOptions{})
	if err != nil {
		return VM{}, err
	}
	defer func() {
		if !created {
			a.rollback(ctx, "VirtualMachine", ns, name)
		}
	}()
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(vm, vmKind)}
	if req.UserData != "" {
		if _, err := a.Kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: a.meta(ns, name+"-cloudinit", owner),
			Data:       map[string][]byte{"userdata": []byte(req.UserData)},
		}, metav1.CreateOptions{}); err != nil {
			return VM{}, err
		}
	}
	if err := a.attach(ctx, ns, name, owner, spec, req.Ports); err != nil {
		return VM{}, err
	}
	created = true
	return a.GetVM(ctx, ns, name)
}

func (a *Adapter) vmObject(ctx context.Context, ns, name string) (*unstructured.Unstructured, error) {
	vm, err := a.Dynamic.Resource(vmResource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || err == nil && (vm.GetLabels()[LabelManaged] != "gpu-adapter" || vm.GetLabels()[labelNode] != a.Node) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("VM %s/%s does not exist", ns, name)}
	}
	return vm, err
}

func (a *Adapter) GetVM(ctx context.Context, ns, name string) (VM, error) {
	vm, err := a.vmObject(ctx, ns, name)
	if err != nil {
		return VM{}, err
	}
	return a.vmView(ctx, vm), nil
}

func (a *Adapter) ListVMs(ctx context.Context, ns, state string) ([]VM, error) {
	list, err := a.Dynamic.Resource(vmResource).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return nil, err
	}
	out := []VM{}
	for i := range list.Items {
		if v := a.vmView(ctx, &list.Items[i]); state == "" || v.Status.State == state {
			out = append(out, v)
		}
	}
	return out, nil
}

func (a *Adapter) vmView(ctx context.Context, vm *unstructured.Unstructured) VM {
	strategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	printable, _, _ := unstructured.NestedString(vm.Object, "status", "printableStatus")
	conditions, _, _ := unstructured.NestedSlice(vm.Object, "status", "conditions")
	var message string
	for _, c := range conditions {
		cond, _ := c.(map[string]any)
		if msg, _ := cond["message"].(string); msg != "" && (cond["status"] == "False" || cond["type"] == "Failure") {
			message = msg
			break
		}
	}
	var status Status
	switch {
	case strategy == "Halted":
		status = Status{State: StateSuspended, Reason: vm.GetAnnotations()[annoReason]}
	case printable == "Running":
		status = Status{State: StateRunning}
	case printable == "ErrorUnschedulable" && strings.Contains(strings.ToLower(message), "claim"):
		status = Status{State: StatePending, Reason: ReasonDeviceTaken, Message: message}
	case slices.Contains([]string{"CrashLoopBackOff", "ErrImagePull", "ImagePullBackOff", "ErrorPvcNotFound", "DataVolumeError", "Unknown"}, printable):
		status = Status{State: StateFailed, Reason: printable, Message: message}
	default:
		status = Status{State: StatePending, Reason: printable, Message: message}
	}
	v := VM{Namespace: vm.GetNamespace(), Name: vm.GetName(), Status: status, GPUs: []string{}, Endpoint: a.endpoint(ctx, vm.GetNamespace(), vm.GetName())}
	if status.State != StateSuspended {
		_ = json.Unmarshal([]byte(vm.GetAnnotations()[annoGPUs]), &v.GPUs)
	}
	return v
}

func (a *Adapter) StopVM(ctx context.Context, ns, name string) (VM, error) {
	vm, err := a.vmObject(ctx, ns, name)
	if err != nil {
		return VM{}, err
	}
	v := a.vmView(ctx, vm)
	if !slices.Contains([]string{StatePending, StateRunning, StateFailed}, v.Status.State) {
		return VM{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("VM %s/%s is %s", ns, name, v.Status.State)}
	}
	if err := a.updateVM(ctx, ns, name, "Halted", "", nil); err != nil {
		return VM{}, err
	}
	return a.GetVM(ctx, ns, name)
}

func (a *Adapter) UpdateVM(ctx context.Context, ns, name string, pcis []string) (VM, error) {
	unlockVM, err := a.lock(true, ns+"/"+name)
	if err != nil {
		return VM{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("VM %s/%s is being started or updated by another request", ns, name)}
	}
	defer unlockVM()
	vm, err := a.vmObject(ctx, ns, name)
	if err != nil {
		return VM{}, err
	}
	if v := a.vmView(ctx, vm); v.Status.State != StateSuspended {
		return VM{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("VM %s/%s is %s, not SUSPENDED", ns, name, v.Status.State)}
	}
	pcis = lower(pcis)
	for i, pci := range pcis {
		if slices.Index(pcis, pci) != i {
			return VM{}, &Error{Code: CodeInvalidSpec, Message: "gpus has duplicate items"}
		}
	}
	nodeGPUs, err := a.ListGPUs(ctx)
	if err != nil {
		return VM{}, err
	}
	if _, err := vmGPUs(nodeGPUs, pcis); err != nil {
		return VM{}, err
	}
	if err := a.updateVM(ctx, ns, name, "Halted", vm.GetAnnotations()[annoReason], pcis); err != nil {
		return VM{}, err
	}
	return a.GetVM(ctx, ns, name)
}

func (a *Adapter) StartVM(ctx context.Context, ns, name string) (VM, error) {
	unlockVM, err := a.lock(true, ns+"/"+name)
	if err != nil {
		return VM{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("VM %s/%s is being started or updated by another request", ns, name)}
	}
	defer unlockVM()
	vm, err := a.vmObject(ctx, ns, name)
	if err != nil {
		return VM{}, err
	}
	if v := a.vmView(ctx, vm); v.Status.State != StateSuspended {
		return VM{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("VM %s/%s is %s, not SUSPENDED", ns, name, v.Status.State)}
	}
	var pcis []string
	_ = json.Unmarshal([]byte(vm.GetAnnotations()[annoGPUs]), &pcis)
	spec, release, err := a.vmClaim(ctx, pcis)
	if err != nil {
		return VM{}, err
	}
	started := false
	defer func() { release(!started) }()
	if err := a.replaceClaim(ctx, ns, name, []metav1.OwnerReference{*metav1.NewControllerRef(vm, vmKind)}, spec); err != nil {
		return VM{}, err
	}
	if err := a.updateVM(ctx, ns, name, "Always", "", nil); err != nil {
		return VM{}, err
	}
	started = true
	return a.GetVM(ctx, ns, name)
}

func (a *Adapter) DeleteVM(ctx context.Context, ns, name string) error {
	if _, err := a.vmObject(ctx, ns, name); err != nil {
		return err
	}
	return a.Dynamic.Resource(vmResource).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationBackground)})
}

func (a *Adapter) updateVM(ctx context.Context, ns, name, strategy, reason string, pcis []string) error {
	vms := a.Dynamic.Resource(vmResource).Namespace(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm, err := vms.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedField(vm.Object, strategy, "spec", "runStrategy"); err != nil {
			return err
		}
		setAnnotation(vm, annoReason, reason)
		if pcis != nil {
			raw, _ := json.Marshal(pcis)
			setAnnotation(vm, annoGPUs, string(raw))
			if err := unstructured.SetNestedSlice(vm.Object, vmDevices(len(pcis)), "spec", "template", "spec", "domain", "devices", "gpus"); err != nil {
				return err
			}
		}
		_, err = vms.Update(ctx, vm, metav1.UpdateOptions{})
		return err
	})
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
