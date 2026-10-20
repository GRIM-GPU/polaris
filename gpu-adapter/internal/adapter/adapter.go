package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"go.uber.org/zap"
)

const (
	ModeFull        = "FULL"
	ModeMIG         = "MIG"
	ModeTimeSlicing = "TIME_SLICING"

	CodeInvalidSpec       = "INVALID_SPEC"
	CodeNotFound          = "NOT_FOUND"
	CodeConflict          = "CONFLICT"
	CodeInvalidState      = "INVALID_STATE"
	CodeNotSupported      = "NOT_SUPPORTED"
	CodeDeviceUnavailable = "DEVICE_UNAVAILABLE"
	CodeDeviceBusy        = "DEVICE_BUSY"
	CodePartitionFailed   = "PARTITION_FAILED"
	CodeKubernetes        = "KUBERNETES_ERROR"
	CodeTimeout           = "TIMEOUT"

	LabelManaged   = "app.kubernetes.io/managed-by"
	labelNode      = "gpu-adapter/node"
	labelWorkload  = "gpu-adapter/workload"
	annoGPUs       = "gpu-adapter/gpus"
	annoReason     = "gpu-adapter/reason"
	PartitionTaint = "gpu-adapter/partitioning"

	DeviceGPU  = "gpu"
	DeviceMIG  = "mig"
	DeviceVFIO = "vfio"
)

type GPU struct {
	GpuID        string
	PciBusID     string
	Model        string
	MemoryBytes  int64
	Binding      string
	Mode         string
	MigInstances []MigInstance
	MigProfiles  []MigProfile
	TimeSlicing  bool
	VmAttachable bool

	uuid       string
	minor      int
	gpuDevice  string
	gpuMemory  int64
	vfioDevice string
	migDevices map[string]string
}

type MigInstance struct {
	InstanceID string
	Profile    string
}

type MigProfile struct {
	Name        string
	MemoryBytes int64
}

type HostGPU struct {
	PciBusID   string
	Driver     string
	IOMMUAlone bool
}

type VendorGPU struct {
	PciBusID     string
	UUID         string
	Model        string
	MemoryBytes  int64
	Minor        int
	MigEnabled   bool
	MigProfiles  []MigProfile
	MigInstances []MigInstance
}

type Vendor interface {
	Open() (Session, error)
	DRA() DRA
	Device(d resourcev1.Device) Device
}

type DRA struct {
	Driver        string
	Classes       map[string]string
	UUIDAttribute string
	Memory        resourcev1.QualifiedName
	TimeSlicing   runtime.RawExtension
	VFIO          runtime.RawExtension
}

type Device struct {
	Kind    string
	UUID    string
	Product string
}

type Session interface {
	Close()
	GPUs() ([]VendorGPU, error)
	ComputeProcesses(uuid string) ([]int, error)
	SetMigMode(pci string, enabled bool) error
	CreateMigInstance(pci, profile string) error
	DeleteMigInstance(uuid string) error
	SetPersistence(pci string, enabled bool) error
	SetDrain(pci string, enabled bool) error
}

type Host interface {
	GPUs() ([]HostGPU, error)
	Holders(minor int) ([]int, error)
	Kill(pids []int) error
}

type Adapter struct {
	Kube            kubernetes.Interface
	Dynamic         dynamic.Interface
	Vendor          Vendor
	Host            Host
	Node            string
	Namespace       string
	PluginNamespace string
	PluginSelector  string

	locks sync.Map
}

type Error struct {
	Code    string
	Message string
	Busy    *BusyDetail
}

type BusyDetail struct {
	Workloads []WorkloadRef
}

type WorkloadRef struct {
	Type      string
	Namespace string
	Name      string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (a *Adapter) ListGPUs(ctx context.Context) ([]GPU, error) {
	hostGPUs, err := a.Host.GPUs()
	if err != nil {
		return nil, fmt.Errorf("read PCI devices: %w", err)
	}
	var vendorGPUs []VendorGPU
	if s, err := a.Vendor.Open(); err == nil {
		vendorGPUs, err = s.GPUs()
		s.Close()
		if err != nil {
			return nil, err
		}
	} else {
		zap.L().Debug("cannot open NVML; GPUs bound to vfio-pci are not visible to it", zap.Error(err))
	}
	slicesList, err := a.Kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var timeSlicing []string
	record, err := a.Kube.CoreV1().ConfigMaps(a.Namespace).Get(ctx, "gpu-adapter-"+a.Node, metav1.GetOptions{})
	if err == nil {
		_ = json.Unmarshal([]byte(record.Data["timeSlicing"]), &timeSlicing)
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	type device struct {
		Device
		name   string
		memory int64
		shared bool
	}
	dra := a.Vendor.DRA()
	devices := map[string][]device{}
	for _, rs := range slicesList.Items {
		if rs.Spec.Driver != dra.Driver || rs.Spec.NodeName == nil || *rs.Spec.NodeName != a.Node {
			continue
		}
		for _, d := range rs.Spec.Devices {
			dev := device{Device: a.Vendor.Device(d), name: d.Name, shared: d.AllowMultipleAllocations != nil && *d.AllowMultipleAllocations}
			if c, ok := d.Capacity[dra.Memory]; ok {
				dev.memory = c.Value.Value()
			}
			pci := strings.ToLower(attr(d, "resource.kubernetes.io/pciBusID"))
			devices[pci] = append(devices[pci], dev)
		}
	}

	out := []GPU{}
	for _, h := range hostGPUs {
		g := GPU{PciBusID: h.PciBusID, Binding: "host", MigProfiles: []MigProfile{}}
		migEnabled := false
		if i := slices.IndexFunc(vendorGPUs, func(v VendorGPU) bool { return v.PciBusID == h.PciBusID }); i >= 0 {
			v := vendorGPUs[i]
			g.uuid, g.Model, g.MemoryBytes, g.minor = v.UUID, v.Model, v.MemoryBytes, v.Minor
			g.MigProfiles = append(g.MigProfiles, v.MigProfiles...)
			if migEnabled = v.MigEnabled; migEnabled {
				g.MigInstances = v.MigInstances
			}
		}
		if h.Driver == "vfio-pci" {
			g.Binding = "vm"
		} else {
			g.GpuID = g.uuid
		}
		g.migDevices = map[string]string{}
		for _, d := range devices[h.PciBusID] {
			g.TimeSlicing = g.TimeSlicing || d.shared
			switch d.Kind {
			case DeviceGPU:
				g.gpuDevice, g.gpuMemory = d.name, d.memory
			case DeviceVFIO:
				g.vfioDevice = d.name
			case DeviceMIG:
				g.migDevices[d.UUID] = d.name
			}
			if g.Model == "" {
				g.Model = d.Product
			}
		}
		g.VmAttachable = h.IOMMUAlone
		switch {
		case migEnabled:
			g.Mode = ModeMIG
		case slices.Contains(timeSlicing, h.PciBusID):
			g.Mode = ModeTimeSlicing
		default:
			g.Mode = ModeFull
		}
		out = append(out, g)
	}
	return out, nil
}

func (a *Adapter) GetGPU(ctx context.Context, ref string) (GPU, error) {
	gpus, err := a.ListGPUs(ctx)
	if err != nil {
		return GPU{}, err
	}
	for _, g := range gpus {
		if strings.EqualFold(g.PciBusID, ref) || g.GpuID != "" && g.GpuID == ref {
			return g, nil
		}
	}
	return GPU{}, &Error{Code: CodeNotFound, Message: fmt.Sprintf("GPU %s does not exist", ref)}
}

func attr(d resourcev1.Device, name string) string {
	if v, ok := d.Attributes[resourcev1.QualifiedName(name)]; ok && v.StringValue != nil {
		return *v.StringValue
	}
	return ""
}

func (a *Adapter) lock(exclusive bool, pcis ...string) (func(), error) {
	var unlocks []func()
	unlock := func() {
		for _, u := range unlocks {
			u()
		}
	}
	for _, pci := range pcis {
		v, _ := a.locks.LoadOrStore(pci, &sync.RWMutex{})
		m := v.(*sync.RWMutex)
		try, u := m.TryRLock, m.RUnlock
		if exclusive {
			try, u = m.TryLock, m.Unlock
		}
		if !try() {
			unlock()
			return nil, &Error{Code: CodeDeviceBusy, Message: fmt.Sprintf("another request is changing GPU %s", pci)}
		}
		unlocks = append(unlocks, u)
	}
	return unlock, nil
}

func (a *Adapter) users(ctx context.Context, devices []string) ([]WorkloadRef, []corev1.Pod, error) {
	if len(devices) == 0 {
		return nil, nil, nil
	}
	claims, err := a.Kube.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	driver := a.Vendor.DRA().Driver
	var refs []WorkloadRef
	var pods []corev1.Pod
	add := func(r WorkloadRef) {
		if !slices.Contains(refs, r) {
			refs = append(refs, r)
		}
	}
	for _, c := range claims.Items {
		if c.Status.Allocation == nil || !slices.ContainsFunc(c.Status.Allocation.Devices.Results, func(r resourcev1.DeviceRequestAllocationResult) bool {
			return r.Driver == driver && r.Pool == a.Node && slices.Contains(devices, r.Device)
		}) {
			continue
		}
		if owner := metav1.GetControllerOf(&c); c.Labels[LabelManaged] == "gpu-adapter" && owner != nil {
			typ := "container"
			if owner.Kind == "VirtualMachine" {
				typ = "vm"
			}
			add(WorkloadRef{Type: typ, Namespace: c.Namespace, Name: owner.Name})
			continue
		}
		for _, r := range c.Status.ReservedFor {
			if r.Resource != "pods" {
				continue
			}
			pod, err := a.Kube.CoreV1().Pods(c.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			pods = append(pods, *pod)
			ref := WorkloadRef{Type: "container", Namespace: pod.Namespace, Name: pod.Name}
			if vm := pod.Labels["vm.kubevirt.io/name"]; vm != "" {
				ref = WorkloadRef{Type: "vm", Namespace: pod.Namespace, Name: vm}
			} else if owner := metav1.GetControllerOf(pod); owner != nil {
				ref.Name = owner.Name
			}
			add(ref)
		}
	}
	return refs, pods, nil
}

func (a *Adapter) session(f func(Session) error) error {
	s, err := a.Vendor.Open()
	if err != nil {
		return err
	}
	defer s.Close()
	return f(s)
}
