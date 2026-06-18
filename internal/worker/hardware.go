package worker

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/orchestra-hq/atlas/internal/wire"
)

// Detect returns the hardware inventory of the current machine. GPU detection
// is minimal in M1 phase 1 (CUDA presence only, no per-GPU VRAM); it is
// expanded in phase 4 when the scheduler uses the figures for placement.
func Detect() wire.Hardware {
	return wire.Hardware{
		Platform: detectPlatform(),
		RAMBytes: detectRAM(),
		GPUs:     detectGPUs(),
	}
}

func detectPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "metal"
	default:
		// Check for NVIDIA CUDA by probing nvidia-smi; not guaranteed present
		// even on CUDA hosts, but is the lowest-overhead probe that works
		// without CGO or GPU driver bindings.
		if _, err := exec.LookPath("nvidia-smi"); err == nil {
			return "cuda"
		}
		return "cpu"
	}
}

func detectRAM() int64 {
	switch runtime.GOOS {
	case "darwin":
		return darwinRAM()
	case "linux":
		return linuxRAM()
	default:
		return 0
	}
}

func darwinRAM() int64 {
	// `sysctl -n hw.memsize` outputs total physical RAM as a decimal integer.
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func linuxRAM() int64 {
	// Read MemTotal from /proc/meminfo.
	out, err := exec.Command("awk", "/MemTotal/{print $2}", "/proc/meminfo").Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

// detectGPUs returns a basic GPU inventory. Phase 1 returns name-only entries
// (no VRAM) from nvidia-smi on CUDA hosts; Metal and CPU hosts return nil.
// Accurate VRAM figures arrive in phase 4.
func detectGPUs() []wire.GPU {
	if detectPlatform() != "cuda" {
		return nil
	}
	out, err := exec.Command("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var gpus []wire.GPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, ", ", 2)
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		var vram int64
		if len(parts) == 2 {
			// nvidia-smi reports MiB without units when --format=nounits.
			if mb, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
				vram = mb << 20
			}
		}
		gpus = append(gpus, wire.GPU{Name: name, VRAMBytes: vram})
	}
	return gpus
}
