package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

type ContainerGPU struct {
	Mode           string `json:"mode"`
	GpuID          string `json:"gpuId,omitempty"`
	InstanceID     string `json:"instanceId,omitempty"`
	GpuMemoryBytes int64  `json:"gpuMemoryBytes,omitempty"`
}

type ContainerCreate struct {
	Common
	ImagePullAuth *ImagePullAuth
	Kind          string
	GPUs          []ContainerGPU
	Command       []string
	Args          []string
	Env           []EnvVar
	Volumes       []ContainerVolume
}

type ImagePullAuth struct {
	Username string
	Password string
}

type EnvVar struct {
	Name  string
	Value string
}

type ContainerVolume struct {
	Name      string
	MountPath string
}

type Container struct {
	Namespace string
	Name      string
	Kind      string
	Status    Status
	GPUs      []ContainerGPU
	Endpoint  string
}

func (a *Adapter) containerClaim(ctx context.Context, gpus []ContainerGPU) (resourcev1.ResourceClaimSpec, func(), error) {
	var spec resourcev1.ResourceClaimSpec
	nodeGPUs, err := a.ListGPUs(ctx)
	if err != nil {
		return spec, nil, err
	}
	var pcis []string
	for _, g := range nodeGPUs {
		if slices.ContainsFunc(gpus, func(cg ContainerGPU) bool {
			return cg.GpuID != "" && cg.GpuID == g.uuid || cg.InstanceID != "" && g.migDevices[cg.InstanceID] != ""
		}) {
			pcis = append(pcis, g.PciBusID)
		}
	}
	unlock, err := a.lock(false, pcis...)
	if err != nil {
		return spec, nil, err
	}
	done := false
	defer func() {
		if !done {
			unlock()
		}
	}()
	if nodeGPUs, err = a.ListGPUs(ctx); err != nil {
		return spec, nil, err
	}
	dra := a.Vendor.DRA()
	for i, cg := range gpus {
		name := fmt.Sprintf("gpu%d", i)
		req := resourcev1.ExactDeviceRequest{DeviceClassName: dra.Classes[DeviceGPU]}
		key := cg.GpuID
		match := func(g GPU) bool { return g.uuid == key }
		switch {
		case cg.Mode == ModeFull && cg.GpuID != "":
		case cg.Mode == ModeMIG && cg.InstanceID != "":
			req.DeviceClassName, key = dra.Classes[DeviceMIG], cg.InstanceID
			match = func(g GPU) bool { return g.migDevices[key] != "" }
		case cg.Mode == ModeTimeSlicing && cg.GpuID != "" && cg.GpuMemoryBytes > 0:
			req.Capacity = &resourcev1.CapacityRequirements{Requests: map[resourcev1.QualifiedName]resource.Quantity{dra.Memory: *resource.NewQuantity(cg.GpuMemoryBytes, resource.BinarySI)}}
			spec.Devices.Config = append(spec.Devices.Config, resourcev1.DeviceClaimConfiguration{
				Requests:            []string{name},
				DeviceConfiguration: resourcev1.DeviceConfiguration{Opaque: &resourcev1.OpaqueDeviceConfiguration{Driver: dra.Driver, Parameters: dra.TimeSlicing}},
			})
		default:
			return spec, nil, &Error{Code: CodeInvalidSpec, Message: fmt.Sprintf("gpus[%d] lacks the fields its mode requires", i)}
		}
		at := slices.IndexFunc(nodeGPUs, match)
		if at < 0 {
			return spec, nil, &Error{Code: CodeDeviceUnavailable, Message: fmt.Sprintf("%s does not exist", key)}
		}
		g := nodeGPUs[at]
		if g.Mode != cg.Mode {
			return spec, nil, &Error{Code: CodeDeviceUnavailable, Message: fmt.Sprintf("GPU %s is in %s mode, not %s", g.PciBusID, g.Mode, cg.Mode)}
		}
		if g.Binding == "vm" {
			refs, _, err := a.users(ctx, slices.DeleteFunc(append([]string{g.gpuDevice, g.vfioDevice}, slices.Collect(maps.Values(g.migDevices))...), func(d string) bool { return d == "" }))
			if err != nil {
				return spec, nil, err
			}
			return spec, nil, &Error{Code: CodeDeviceBusy, Message: fmt.Sprintf("GPU %s is assigned to a VM", g.PciBusID), Busy: &BusyDetail{Workloads: append([]WorkloadRef{}, refs...)}}
		}
		if cg.Mode == ModeFull && g.gpuMemory > 0 {
			req.Capacity = &resourcev1.CapacityRequirements{Requests: map[resourcev1.QualifiedName]resource.Quantity{dra.Memory: *resource.NewQuantity(g.gpuMemory, resource.BinarySI)}}
		}
		req.Selectors = []resourcev1.DeviceSelector{{CEL: &resourcev1.CELDeviceSelector{Expression: fmt.Sprintf(`device.attributes[%q].%s == %q`, dra.Driver, dra.UUIDAttribute, key)}}}
		spec.Devices.Requests = append(spec.Devices.Requests, resourcev1.DeviceRequest{Name: name, Exactly: &req})
	}
	done = true
	return spec, unlock, nil
}

func (a *Adapter) CreateContainer(ctx context.Context, req ContainerCreate) (Container, error) {
	if req.Image == "" {
		return Container{}, &Error{Code: CodeInvalidSpec, Message: "image is required"}
	}
	if p := req.ImagePullAuth; p != nil && (p.Username == "" || p.Password == "") {
		return Container{}, &Error{Code: CodeInvalidSpec, Message: "imagePullAuth needs username and password"}
	}
	ns, name := req.Namespace, req.Name
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(req.CPUMillicores, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(req.MemoryBytes, resource.BinarySI),
	}
	c := corev1.Container{
		Name:    "main",
		Image:   req.Image,
		Command: req.Command,
		Args:    req.Args,
		Resources: corev1.ResourceRequirements{
			Requests: limits,
			Limits:   limits,
			Claims:   []corev1.ResourceClaim{{Name: "gpu"}},
		},
	}
	pod := corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: new(name + "-gpu")}}}
	for _, e := range req.Env {
		if e.Name == "" {
			return Container{}, &Error{Code: CodeInvalidSpec, Message: "env needs a name"}
		}
		c.Env = append(c.Env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	for _, p := range req.Ports {
		c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: int32(p)})
	}
	var volumes []string
	for _, v := range req.Volumes {
		if v.Name == "" || v.MountPath == "" {
			return Container{}, &Error{Code: CodeInvalidSpec, Message: "volumes need name and mountPath"}
		}
		volumes = append(volumes, v.Name)
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: v.Name, MountPath: v.MountPath})
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: v.Name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: v.Name}}})
	}
	pod.Containers = []corev1.Container{c}
	if req.ImagePullAuth != nil {
		pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: name + "-pull"}}
	}

	spec, unlock, err := a.containerClaim(ctx, req.GPUs)
	if err != nil {
		return Container{}, err
	}
	defer unlock()
	if err := a.prepare(ctx, ns, name, "container", volumes); err != nil {
		return Container{}, err
	}

	meta := a.meta(ns, name, nil)
	gpus, _ := json.Marshal(req.GPUs)
	meta.Annotations = map[string]string{annoGPUs: string(gpus)}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelWorkload: name}}, Spec: pod}
	var owner []metav1.OwnerReference
	created := false
	defer func() {
		if !created && owner != nil {
			a.rollback(ctx, owner[0].Kind, ns, name)
		}
	}()
	if req.Kind == "service" {
		d, err := a.Kube.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: template.Labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: template,
		}}, metav1.CreateOptions{})
		if err != nil {
			return Container{}, err
		}
		owner = []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))}
	} else {
		template.Spec.RestartPolicy = corev1.RestartPolicyNever
		j, err := a.Kube.BatchV1().Jobs(ns).Create(ctx, &batchv1.Job{ObjectMeta: meta, Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), Template: template}}, metav1.CreateOptions{})
		if err != nil {
			return Container{}, err
		}
		owner = []metav1.OwnerReference{*metav1.NewControllerRef(j, batchv1.SchemeGroupVersion.WithKind("Job"))}
	}
	if p := req.ImagePullAuth; p != nil {
		registry := strings.Split(req.Image, "/")[0]
		if !strings.ContainsAny(registry, ".:") && registry != "localhost" {
			registry = "https://index.docker.io/v1/"
		}
		auth := base64.StdEncoding.EncodeToString([]byte(p.Username + ":" + p.Password))
		config, _ := json.Marshal(map[string]any{"auths": map[string]any{registry: map[string]string{"username": p.Username, "password": p.Password, "auth": auth}}})
		if _, err := a.Kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: a.meta(ns, name+"-pull", owner),
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: config},
		}, metav1.CreateOptions{}); err != nil {
			return Container{}, err
		}
	}
	if err := a.attach(ctx, ns, name, owner, spec, req.Ports); err != nil {
		return Container{}, err
	}
	created = true
	return a.GetContainer(ctx, ns, name)
}

func (a *Adapter) containerObject(ctx context.Context, ns, name string) (*appsv1.Deployment, *batchv1.Job, error) {
	d, err := a.Kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil && (d.GetLabels()[LabelManaged] == "gpu-adapter" && d.GetLabels()[labelNode] == a.Node) {
		return d, nil, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	j, err := a.Kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil && (j.GetLabels()[LabelManaged] == "gpu-adapter" && j.GetLabels()[labelNode] == a.Node) {
		return nil, j, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	return nil, nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("container workload %s/%s does not exist", ns, name)}
}

func (a *Adapter) GetContainer(ctx context.Context, ns, name string) (Container, error) {
	d, j, err := a.containerObject(ctx, ns, name)
	if err != nil {
		return Container{}, err
	}
	return a.containerView(ctx, d, j)
}

func (a *Adapter) ListContainers(ctx context.Context, ns, state string) ([]Container, error) {
	deps, err := a.Kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return nil, err
	}
	jobs, err := a.Kube.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=gpu-adapter," + labelNode + "=" + a.Node})
	if err != nil {
		return nil, err
	}
	out := []Container{}
	for i := range len(deps.Items) + len(jobs.Items) {
		var c Container
		if i < len(deps.Items) {
			c, err = a.containerView(ctx, &deps.Items[i], nil)
		} else {
			c, err = a.containerView(ctx, nil, &jobs.Items[i-len(deps.Items)])
		}
		if err != nil {
			return nil, err
		}
		if state == "" || c.Status.State == state {
			out = append(out, c)
		}
	}
	return out, nil
}

func (a *Adapter) containerView(ctx context.Context, d *appsv1.Deployment, j *batchv1.Job) (Container, error) {
	var meta *metav1.ObjectMeta
	var status Status
	kind := "service"
	if d != nil {
		meta = &d.ObjectMeta
		if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
			status = Status{State: StateSuspended, Reason: meta.Annotations[annoReason]}
		}
	} else {
		meta, kind = &j.ObjectMeta, "job"
		for _, c := range j.Status.Conditions {
			if c.Status == corev1.ConditionTrue && c.Type == batchv1.JobComplete {
				status = Status{State: StateCompleted}
			}
			if c.Status == corev1.ConditionTrue && c.Type == batchv1.JobFailed {
				status = Status{State: StateFailed, Reason: c.Reason, Message: c.Message}
			}
		}
		if status.State == "" && j.Spec.Suspend != nil && *j.Spec.Suspend {
			status = Status{State: StateSuspended, Reason: meta.Annotations[annoReason]}
		}
	}
	if status.State == "" {
		pods, err := a.Kube.CoreV1().Pods(meta.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labelWorkload + "=" + meta.Name})
		if err != nil {
			return Container{}, err
		}
		var pod *corev1.Pod
		for i := range pods.Items {
			if pods.Items[i].DeletionTimestamp == nil && (pod == nil || pod.CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)) {
				pod = &pods.Items[i]
			}
		}
		status = Status{State: StatePending}
		if pod != nil {
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
					status = Status{State: StatePending, Reason: c.Reason, Message: c.Message}
					if strings.Contains(strings.ToLower(c.Message), "claim") {
						status.Reason = ReasonDeviceTaken
					}
				}
			}
			switch pod.Status.Phase {
			case corev1.PodRunning, corev1.PodSucceeded:
				status = Status{State: StateRunning}
			case corev1.PodFailed:
				status = Status{State: StateFailed, Reason: pod.Status.Reason, Message: pod.Status.Message}
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && slices.Contains([]string{"CrashLoopBackOff", "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError"}, w.Reason) {
					status = Status{State: StateFailed, Reason: w.Reason, Message: w.Message}
				}
			}
		}
	}
	c := Container{Namespace: meta.Namespace, Name: meta.Name, Kind: kind, Status: status, GPUs: []ContainerGPU{}, Endpoint: a.endpoint(ctx, meta.Namespace, meta.Name)}
	if status.Reason != ReasonDeviceRemoved {
		_ = json.Unmarshal([]byte(meta.Annotations[annoGPUs]), &c.GPUs)
	}
	return c, nil
}

func (a *Adapter) StartContainer(ctx context.Context, ns, name string, gpus []ContainerGPU) (Container, error) {
	unlockWorkload, err := a.lock(true, ns+"/"+name)
	if err != nil {
		return Container{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("container workload %s/%s is being started by another request", ns, name)}
	}
	defer unlockWorkload()
	d, j, err := a.containerObject(ctx, ns, name)
	if err != nil {
		return Container{}, err
	}
	cur, err := a.containerView(ctx, d, j)
	if err != nil {
		return Container{}, err
	}
	if cur.Status.State != StateSuspended {
		return Container{}, &Error{Code: CodeInvalidState, Message: fmt.Sprintf("container workload %s/%s is %s, not SUSPENDED", ns, name, cur.Status.State)}
	}
	spec, unlock, err := a.containerClaim(ctx, gpus)
	if err != nil {
		return Container{}, err
	}
	defer unlock()
	var owner []metav1.OwnerReference
	if d != nil {
		owner = []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))}
	} else {
		owner = []metav1.OwnerReference{*metav1.NewControllerRef(j, batchv1.SchemeGroupVersion.WithKind("Job"))}
	}
	if err := a.replaceClaim(ctx, ns, name, owner, spec); err != nil {
		return Container{}, err
	}
	raw, _ := json.Marshal(gpus)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if d != nil {
			d, err := a.Kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			setAnnotation(d, annoGPUs, string(raw))
			setAnnotation(d, annoReason, "")
			d.Spec.Replicas = new(int32(1))
			_, err = a.Kube.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{})
			return err
		}
		j, err := a.Kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		setAnnotation(j, annoGPUs, string(raw))
		setAnnotation(j, annoReason, "")
		j.Spec.Suspend = new(false)
		_, err = a.Kube.BatchV1().Jobs(ns).Update(ctx, j, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return Container{}, err
	}
	return a.GetContainer(ctx, ns, name)
}

func (a *Adapter) DeleteContainer(ctx context.Context, ns, name string) error {
	d, _, err := a.containerObject(ctx, ns, name)
	if err != nil {
		return err
	}
	opts := metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationBackground)}
	if d != nil {
		return a.Kube.AppsV1().Deployments(ns).Delete(ctx, name, opts)
	}
	return a.Kube.BatchV1().Jobs(ns).Delete(ctx, name, opts)
}
