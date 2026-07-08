package recipe

import (
	"fmt"
	"strings"

	"github.com/isannai/isann-servers/pkg/setup"
)

// Check detects the local hardware and returns the list of unmet `requires`
// items (empty slice = all pass). nil Requires passes trivially.
func (r *Requires) Check() []string {
	if r == nil {
		return nil
	}
	return r.CheckAgainst(setup.DetectHardwareStatic(""))
}

// CheckAgainst compares the requirements against a given hardware snapshot.
// Split out from Check so tests can inject a HardwareSpec without shelling
// out to nvidia-smi.
func (r *Requires) CheckAgainst(hw setup.HardwareSpec) []string {
	if r == nil {
		return nil
	}
	var failed []string

	// vram: the largest single GPU's total VRAM must be >= the requirement.
	if r.VRAMGB > 0 {
		have := 0.0
		for _, g := range hw.GPUs {
			if g.VramTotalGB > have {
				have = g.VramTotalGB
			}
		}
		if have < r.VRAMGB {
			failed = append(failed, fmt.Sprintf("vram: %sG+ → have %sG", trimFloat(r.VRAMGB), trimFloat(have)))
		}
	}

	// gpu: at least one GPU name must contain one of the patterns (case-insensitive).
	if len(r.GPUPatterns) > 0 {
		matched := false
		for _, g := range hw.GPUs {
			name := strings.ToLower(g.Name)
			for _, p := range r.GPUPatterns {
				if strings.Contains(name, strings.ToLower(p)) {
					matched = true
				}
			}
		}
		if !matched {
			failed = append(failed, fmt.Sprintf("gpu: %s → have %q",
				strings.Join(r.GPUPatterns, " | "), gpuNames(hw)))
		}
	}

	return failed
}

// gpuNames joins detected GPU names for the failure message ("none" when no GPU).
func gpuNames(hw setup.HardwareSpec) string {
	if len(hw.GPUs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(hw.GPUs))
	for _, g := range hw.GPUs {
		names = append(names, g.Name)
	}
	return strings.Join(names, ", ")
}

// trimFloat formats a float without a trailing ".0" (8.0 → "8", 7.5 → "7.5").
func trimFloat(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	s = strings.TrimSuffix(s, ".0")
	return s
}
