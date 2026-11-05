package adapter

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	"go.uber.org/zap"
)

const (
	StatePending   = "PENDING"
	StateRunning   = "RUNNING"
	StateSuspended = "SUSPENDED"
	StateCompleted = "COMPLETED"
	StateFailed    = "FAILED"

	ReasonDeviceTaken   = "DEVICE_TAKEN"
	ReasonDeviceRemoved = "DEVICE_REMOVED"
)

var (
	vmResource = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	vmKind     = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}
)

type Status struct {
	State   string
	Reason  string
	Message string
}

type Common struct {
	Namespace     string
	Name          string
	Image         string
	CPUMillicores int64
	MemoryBytes   int64
	Ports         []int
}

func (a *Adapter) meta(ns, name string, owner []metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name, Namespace: ns, OwnerReferences: owner,
		Labels: map[string]string{LabelManaged: "gpu-adapter", labelNode: a.Node},
	}
}

func (a *Adapter) prepare(ctx context.Context, ns, name, typ string, volumes []string) error {
	for _, get := range []func() error{
		func() error {
			_, err := a.Kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		},
		func() error { _, err := a.Kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{}); return err },
		func() error {
			_, err := a.Dynamic.Resource(vmResource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		},
	} {
		if err := get(); err == nil {
			return &Error{Code: CodeConflict, Message: fmt.Sprintf("workload %s/%s already exists", ns, name)}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	if len(volumes) > 0 {
		users, err := a.volumeUsers(ctx, ns)
		if err != nil {
			return err
		}
		for _, v := range volumes {
			pvc, err := a.Kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, v, metav1.GetOptions{})
			if apierrors.IsNotFound(err) || err == nil && (pvc.GetLabels()[LabelManaged] != "gpu-adapter" || pvc.GetLabels()[labelNode] != a.Node) {
				return &Error{Code: CodeInvalidSpec, Message: fmt.Sprintf("volume %s/%s does not exist", ns, v)}
			}
			if err != nil {
				return err
			}
			for _, u := range users[v] {
				if typ == "vm" || u.Type == "vm" {
					return &Error{Code: CodeInvalidState, Message: fmt.Sprintf("volume %s/%s is attached to %s %s", ns, v, u.Type, u.Name)}
				}
			}
		}
	}
	_, err := a.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (a *Adapter) volumeUsers(ctx context.Context, ns string) (map[string][]WorkloadRef, error) {
	out := map[string][]WorkloadRef{}
	add := func(ref WorkloadRef, vols []corev1.Volume) {
		for _, v := range vols {
			if v.PersistentVolumeClaim != nil {
				out[v.PersistentVolumeClaim.ClaimName] = append(out[v.PersistentVolumeClaim.ClaimName], ref)
			}
		}
	}
	deps, err := a.Kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return nil, err
	}
	for _, d := range deps.Items {
		add(WorkloadRef{"container", ns, d.Name}, d.Spec.Template.Spec.Volumes)
	}
	jobs, err := a.Kube.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return nil, err
	}
	for _, j := range jobs.Items {
		add(WorkloadRef{"container", ns, j.Name}, j.Spec.Template.Spec.Volumes)
	}
	vms, err := a.Dynamic.Resource(vmResource).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return nil, err
	}
	for _, vm := range vms.Items {
		vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
		for _, v := range vols {
			if claim, ok, _ := unstructured.NestedString(v.(map[string]any), "persistentVolumeClaim", "claimName"); ok {
				out[claim] = append(out[claim], WorkloadRef{"vm", ns, vm.GetName()})
			}
		}
	}
	return out, nil
}

func (a *Adapter) attach(ctx context.Context, ns, name string, owner []metav1.OwnerReference, spec resourcev1.ResourceClaimSpec, ports []int) error {
	if _, err := a.Kube.ResourceV1().ResourceClaims(ns).Create(ctx, &resourcev1.ResourceClaim{ObjectMeta: a.meta(ns, name+"-gpu", owner), Spec: spec}, metav1.CreateOptions{}); err != nil {
		return err
	}
	if len(ports) == 0 {
		return nil
	}
	svc := &corev1.Service{ObjectMeta: a.meta(ns, name, owner), Spec: corev1.ServiceSpec{Selector: map[string]string{labelWorkload: name}}}
	for _, p := range ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("tcp-%d", p), Port: int32(p), TargetPort: intstr.FromInt(p)})
	}
	_, err := a.Kube.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{})
	return err
}

func (a *Adapter) replaceClaim(ctx context.Context, ns, name string, owner []metav1.OwnerReference, spec resourcev1.ResourceClaimSpec) error {
	claims := a.Kube.ResourceV1().ResourceClaims(ns)
	if err := claims.Delete(ctx, name+"-gpu", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	_, err := claims.Create(ctx, &resourcev1.ResourceClaim{ObjectMeta: a.meta(ns, name+"-gpu", owner), Spec: spec}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return &Error{Code: CodeInvalidState, Message: fmt.Sprintf("ResourceClaim %s/%s-gpu is still being deleted; retry the request", ns, name)}
	}
	return err
}

func (a *Adapter) rollback(ctx context.Context, kind, ns, name string) {
	ctx = context.WithoutCancel(ctx)
	opts := metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationBackground)}
	var err error
	switch kind {
	case "Deployment":
		err = a.Kube.AppsV1().Deployments(ns).Delete(ctx, name, opts)
	case "Job":
		err = a.Kube.BatchV1().Jobs(ns).Delete(ctx, name, opts)
	case "VirtualMachine":
		err = a.Dynamic.Resource(vmResource).Namespace(ns).Delete(ctx, name, opts)
	}
	if err != nil {
		zap.L().Warn("cannot remove object left by a failed request", zap.String("kind", kind), zap.String("namespace", ns), zap.String("name", name), zap.Error(err))
	}
}

func (a *Adapter) endpoint(ctx context.Context, ns, name string) string {
	if _, err := a.Kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		return ""
	}
	return name + "." + ns + ".svc"
}

func setAnnotation(o metav1.Object, key, value string) {
	annotations := o.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if value == "" {
		delete(annotations, key)
	} else {
		annotations[key] = value
	}
	o.SetAnnotations(annotations)
}
