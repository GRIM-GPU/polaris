package adapter

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Volume struct {
	Namespace string
	Name      string
	SizeBytes int64
}

func (a *Adapter) CreateVolume(ctx context.Context, v Volume) (Volume, error) {
	_, err := a.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: v.Namespace}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return Volume{}, err
	}
	_, err = a.Kube.CoreV1().PersistentVolumeClaims(v.Namespace).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: a.meta(v.Namespace, v.Name, nil),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(v.SizeBytes, resource.BinarySI)}},
		},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return Volume{}, &Error{Code: CodeConflict, Message: fmt.Sprintf("volume %s/%s already exists", v.Namespace, v.Name)}
	}
	return v, err
}

func (a *Adapter) ListVolumes(ctx context.Context, ns string) ([]Volume, error) {
	list, err := a.Kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return nil, err
	}
	out := []Volume{}
	for _, p := range list.Items {
		size := p.Spec.Resources.Requests[corev1.ResourceStorage]
		out = append(out, Volume{Namespace: p.Namespace, Name: p.Name, SizeBytes: size.Value()})
	}
	return out, nil
}

func (a *Adapter) DeleteVolume(ctx context.Context, ns, name string) error {
	pvc, err := a.Kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || err == nil && (pvc.GetLabels()[LabelManaged] != "gpu-adapter" || pvc.GetLabels()[labelNode] != a.Node) {
		return &Error{Code: CodeNotFound, Message: fmt.Sprintf("volume %s/%s does not exist", ns, name)}
	}
	if err != nil {
		return err
	}
	users, err := a.volumeUsers(ctx, ns)
	if err != nil {
		return err
	}
	if len(users[name]) > 0 {
		return &Error{Code: CodeInvalidState, Message: fmt.Sprintf("volume %s/%s is used by %s %s", ns, name, users[name][0].Type, users[name][0].Name)}
	}
	return a.Kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
}
