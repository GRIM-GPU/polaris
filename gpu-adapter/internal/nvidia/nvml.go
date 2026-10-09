package nvidia

import (
	"fmt"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"gpu-adapter/internal/adapter"
)

const (
	PCIVendor   = "0x10de"
	DeviceMajor = 195
)

var Daemons = []string{"nvidia-persistenced"}

type NVML struct {
	Lib nvml.Interface
	mu  sync.Mutex
}

func (n *NVML) DRA() adapter.DRA {
	return adapter.DRA{
		Driver:        "gpu.nvidia.com",
		Classes:       map[string]string{adapter.DeviceGPU: "gpu.nvidia.com", adapter.DeviceMIG: "mig.nvidia.com", adapter.DeviceVFIO: "vfio.gpu.nvidia.com"},
		UUIDAttribute: "uuid",
		Memory:        "memory",
		TimeSlicing:   runtime.RawExtension{Raw: []byte(`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"GpuConfig","sharing":{"strategy":"TimeSlicing"}}`)},
		VFIO:          runtime.RawExtension{Raw: []byte(`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"VfioDeviceConfig","iommu":{"backendPolicy":"LegacyOnly","enableAPIDevice":true}}`)},
	}
}

func (n *NVML) Device(d resourcev1.Device) adapter.Device {
	var out adapter.Device
	for name, v := range d.Attributes {
		if v.StringValue == nil {
			continue
		}
		switch name {
		case "type":
			out.Kind = *v.StringValue
		case "uuid":
			out.UUID = *v.StringValue
		case "productName":
			out.Product = *v.StringValue
		}
	}
	return out
}

type session struct {
	n *NVML
}

func (n *NVML) Open() (adapter.Session, error) {
	n.mu.Lock()
	if ret := n.Lib.Init(); ret != nvml.SUCCESS {
		n.mu.Unlock()
		return nil, failed("initialize NVML", ret)
	}
	return &session{n: n}, nil
}

func (s *session) Close() {
	_ = s.n.Lib.Shutdown()
	s.n.mu.Unlock()
}

func (s *session) GPUs() ([]adapter.VendorGPU, error) {
	count, ret := s.n.Lib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, failed("count GPUs", ret)
	}
	var out []adapter.VendorGPU
	for i := range count {
		dev, ret := s.n.Lib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, failed("get GPU", ret)
		}
		pci, ret := dev.GetPciInfo()
		if ret != nvml.SUCCESS {
			return nil, failed("get PCI info", ret)
		}
		bus := strings.ToLower(cstr(pci.BusId[:]))
		if len(bus) == 16 {
			bus = bus[4:]
		}
		g := adapter.VendorGPU{PciBusID: bus}
		g.UUID, _ = dev.GetUUID()
		g.Model, _ = dev.GetName()
		g.Minor, _ = dev.GetMinorNumber()
		if mem, ret := dev.GetMemoryInfo(); ret == nvml.SUCCESS {
			g.MemoryBytes = int64(mem.Total)
		}
		current, _, ret := dev.GetMigMode()
		migCapable := ret == nvml.SUCCESS
		g.MigEnabled = migCapable && current == nvml.DEVICE_MIG_ENABLE
		profiles := map[uint32]string{}
		if migCapable {
			for p := range nvml.GPU_INSTANCE_PROFILE_COUNT {
				info, ret := dev.GetGpuInstanceProfileInfoV(p).V2()
				if ret != nvml.SUCCESS {
					continue
				}
				name := strings.TrimPrefix(cstr(info.Name[:]), "MIG ")
				profiles[info.Id] = name
				g.MigProfiles = append(g.MigProfiles, adapter.MigProfile{Name: name, MemoryBytes: int64(info.MemorySizeMB) << 20})
			}
		}
		if g.MigEnabled {
			max, _ := dev.GetMaxMigDeviceCount()
			for j := range max {
				mig, ret := dev.GetMigDeviceHandleByIndex(j)
				if ret != nvml.SUCCESS {
					continue
				}
				uuid, _ := mig.GetUUID()
				giID, _ := mig.GetGpuInstanceId()
				gi, ret := dev.GetGpuInstanceById(giID)
				if ret != nvml.SUCCESS {
					return nil, failed("get GPU instance", ret)
				}
				info, _ := gi.GetInfo()
				g.MigInstances = append(g.MigInstances, adapter.MigInstance{InstanceID: uuid, Profile: profiles[info.ProfileId]})
			}
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *session) device(pci string) (nvml.Device, error) {
	dev, ret := s.n.Lib.DeviceGetHandleByPciBusId(pci)
	if ret != nvml.SUCCESS {
		return nil, failed("get GPU "+pci, ret)
	}
	return dev, nil
}

func (s *session) ComputeProcesses(uuid string) ([]int, error) {
	dev, ret := s.n.Lib.DeviceGetHandleByUUID(uuid)
	if ret != nvml.SUCCESS {
		return nil, failed("get "+uuid, ret)
	}
	procs, ret := dev.GetComputeRunningProcesses()
	if ret != nvml.SUCCESS {
		return nil, failed("get compute processes", ret)
	}
	var out []int
	for _, p := range procs {
		out = append(out, int(p.Pid))
	}
	return out, nil
}

func (s *session) SetMigMode(pci string, enabled bool) error {
	dev, err := s.device(pci)
	if err != nil {
		return err
	}
	mode := nvml.DEVICE_MIG_ENABLE
	if !enabled {
		mode = nvml.DEVICE_MIG_DISABLE
		for p := range nvml.GPU_INSTANCE_PROFILE_COUNT {
			info, ret := dev.GetGpuInstanceProfileInfo(p)
			if ret != nvml.SUCCESS {
				continue
			}
			gis, ret := dev.GetGpuInstances(&info)
			if ret != nvml.SUCCESS {
				return failed("list MIG instances", ret)
			}
			for _, gi := range gis {
				for c := range nvml.COMPUTE_INSTANCE_PROFILE_COUNT {
					ciInfo, ret := gi.GetComputeInstanceProfileInfo(c, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
					if ret != nvml.SUCCESS {
						continue
					}
					cis, ret := gi.GetComputeInstances(&ciInfo)
					if ret != nvml.SUCCESS {
						return failed("list compute instances", ret)
					}
					for _, ci := range cis {
						if ret := ci.Destroy(); ret != nvml.SUCCESS {
							return failed("destroy compute instance", ret)
						}
					}
				}
				if ret := gi.Destroy(); ret != nvml.SUCCESS {
					return failed("destroy MIG instance", ret)
				}
			}
		}
	}
	ret, activation := dev.SetMigMode(mode)
	if ret != nvml.SUCCESS {
		return failed("set MIG mode", ret)
	}
	if activation != nvml.SUCCESS {
		return failed("activate MIG mode", activation)
	}
	if current, pending, _ := dev.GetMigMode(); current != pending {
		return &adapter.Error{Code: adapter.CodePartitionFailed, Message: "MIG mode change is pending a GPU reset"}
	}
	return nil
}

func (s *session) CreateMigInstance(pci, profile string) error {
	dev, err := s.device(pci)
	if err != nil {
		return err
	}
	for p := range nvml.GPU_INSTANCE_PROFILE_COUNT {
		if v2, ret := dev.GetGpuInstanceProfileInfoV(p).V2(); ret != nvml.SUCCESS || strings.TrimPrefix(cstr(v2.Name[:]), "MIG ") != profile {
			continue
		}
		info, ret := dev.GetGpuInstanceProfileInfo(p)
		if ret != nvml.SUCCESS {
			return failed("get MIG profile "+profile, ret)
		}
		gi, ret := dev.CreateGpuInstance(&info)
		if ret != nvml.SUCCESS {
			return failed("create MIG instance "+profile, ret)
		}
		for c := range nvml.COMPUTE_INSTANCE_PROFILE_COUNT {
			ci, ret := gi.GetComputeInstanceProfileInfo(c, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
			if ret != nvml.SUCCESS || ci.SliceCount != info.SliceCount {
				continue
			}
			if _, ret := gi.CreateComputeInstance(&ci); ret != nvml.SUCCESS {
				_ = gi.Destroy()
				return failed("create compute instance", ret)
			}
			return nil
		}
		_ = gi.Destroy()
		return &adapter.Error{Code: adapter.CodePartitionFailed, Message: "no compute instance profile fits " + profile}
	}
	return &adapter.Error{Code: adapter.CodeInvalidSpec, Message: "MIG profile " + profile + " does not exist"}
}

func (s *session) DeleteMigInstance(uuid string) error {
	mig, ret := s.n.Lib.DeviceGetHandleByUUID(uuid)
	if ret != nvml.SUCCESS {
		return failed("get MIG instance "+uuid, ret)
	}
	parent, ret := mig.GetDeviceHandleFromMigDeviceHandle()
	if ret != nvml.SUCCESS {
		return failed("get GPU of MIG instance "+uuid, ret)
	}
	giID, _ := mig.GetGpuInstanceId()
	ciID, _ := mig.GetComputeInstanceId()
	gi, ret := parent.GetGpuInstanceById(giID)
	if ret != nvml.SUCCESS {
		return failed("get GPU instance", ret)
	}
	if ci, ret := gi.GetComputeInstanceById(ciID); ret == nvml.SUCCESS {
		if ret := ci.Destroy(); ret != nvml.SUCCESS {
			return failed("destroy compute instance", ret)
		}
	}
	if ret := gi.Destroy(); ret != nvml.SUCCESS {
		return failed("destroy MIG instance "+uuid, ret)
	}
	return nil
}

func (s *session) SetPersistence(pci string, enabled bool) error {
	dev, err := s.device(pci)
	if err != nil {
		return err
	}
	if ret := dev.SetPersistenceMode(state(enabled)); ret != nvml.SUCCESS {
		return failed("set persistence mode", ret)
	}
	return nil
}

func (s *session) SetDrain(pci string, enabled bool) error {
	var info nvml.PciInfo
	var function uint32
	if _, err := fmt.Sscanf(pci, "%x:%x:%x.%x", &info.Domain, &info.Bus, &info.Device, &function); err != nil {
		return err
	}
	for i, c := range fmt.Sprintf("%08X:%02X:%02X.%X", info.Domain, info.Bus, info.Device, function) {
		info.BusId[i] = int8(c)
	}
	if ret := s.n.Lib.DeviceModifyDrainState(&info, state(enabled)); ret != nvml.SUCCESS {
		return failed("set drain state", ret)
	}
	return nil
}

func state(enabled bool) nvml.EnableState {
	if enabled {
		return nvml.FEATURE_ENABLED
	}
	return nvml.FEATURE_DISABLED
}

func failed(what string, ret nvml.Return) error {
	code := adapter.CodePartitionFailed
	if ret == nvml.ERROR_IN_USE {
		code = adapter.CodeDeviceBusy
	}
	return &adapter.Error{Code: code, Message: what + ": " + nvml.ErrorString(ret)}
}

func cstr(b []int8) string {
	var sb strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}
