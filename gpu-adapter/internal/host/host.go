package host

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"gpu-adapter/internal/adapter"

	"go.uber.org/zap"
)

type Host struct {
	Sys         string
	Proc        string
	Grace       time.Duration
	PCIVendor   string
	DeviceMajor uint32
	Keep        []string
}

func (h Host) GPUs() ([]adapter.HostGPU, error) {
	root := filepath.Join(h.Sys, "bus/pci/devices")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []adapter.HostGPU
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		vendor, _ := os.ReadFile(filepath.Join(dir, "vendor"))
		class, _ := os.ReadFile(filepath.Join(dir, "class"))
		c := strings.TrimSpace(string(class))
		if strings.TrimSpace(string(vendor)) != h.PCIVendor || !strings.HasPrefix(c, "0x0300") && !strings.HasPrefix(c, "0x0302") {
			continue
		}
		g := adapter.HostGPU{PciBusID: e.Name()}
		if link, err := os.Readlink(filepath.Join(dir, "driver")); err == nil {
			g.Driver = filepath.Base(link)
		}
		group, err := os.ReadDir(filepath.Join(dir, "iommu_group/devices"))
		g.IOMMUAlone = err == nil && len(group) == 1
		out = append(out, g)
	}
	return out, nil
}

func (h Host) Holders(minor int) ([]int, error) {
	procs, err := os.ReadDir(h.Proc)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if comm, _ := os.ReadFile(filepath.Join(h.Proc, p.Name(), "comm")); slices.Contains(h.Keep, strings.TrimSpace(string(comm))) {
			continue
		}
		fds, _ := os.ReadDir(filepath.Join(h.Proc, p.Name(), "fd"))
		for _, fd := range fds {
			var st unix.Stat_t
			if unix.Stat(filepath.Join(h.Proc, p.Name(), "fd", fd.Name()), &st) == nil &&
				st.Mode&unix.S_IFMT == unix.S_IFCHR && unix.Major(st.Rdev) == h.DeviceMajor && unix.Minor(st.Rdev) == uint32(minor) {
				out = append(out, pid)
				break
			}
		}
	}
	return out, nil
}

func (h Host) Kill(pids []int) error {
	alive := func() []int {
		var out []int
		for _, pid := range pids {
			if _, err := os.Stat(filepath.Join(h.Proc, strconv.Itoa(pid))); err == nil {
				out = append(out, pid)
			}
		}
		return out
	}
	zap.L().Info("sending SIGTERM to processes holding a GPU", zap.Ints("pids", pids))
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for deadline := time.Now().Add(h.Grace); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if len(alive()) == 0 {
			return nil
		}
	}
	left := alive()
	zap.L().Warn("sending SIGKILL to processes that ignored SIGTERM", zap.Ints("pids", left))
	for _, pid := range left {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	time.Sleep(time.Second)
	if left = alive(); len(left) > 0 {
		return fmt.Errorf("processes %v did not exit", left)
	}
	return nil
}
