package worker

import (
	"runtime"
	"testing"
)

func TestDetect_basic(t *testing.T) {
	hw := Detect()

	// Platform must be one of the three known values.
	switch hw.Platform {
	case "cuda", "metal", "cpu":
	default:
		t.Errorf("unexpected platform %q", hw.Platform)
	}

	// On platforms we know how to probe, RAM must be positive.
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if hw.RAMBytes <= 0 {
			t.Errorf("RAMBytes = %d; want > 0 on %s", hw.RAMBytes, runtime.GOOS)
		}
	}

	// GPU slice must be nil or non-empty; no zero-length non-nil slices.
	if hw.GPUs != nil && len(hw.GPUs) == 0 {
		t.Error("GPUs is non-nil but empty; should be nil when no GPUs detected")
	}
}

func TestDetect_platform_matches_os(t *testing.T) {
	hw := Detect()
	if runtime.GOOS == "darwin" && hw.Platform != "metal" {
		t.Errorf("on darwin expected platform=metal, got %q", hw.Platform)
	}
}
