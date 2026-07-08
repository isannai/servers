package rendezvous

import (
	"testing"

	"github.com/isannai/isann-servers/pkg/setup"
)

// The RV-side hardware hash (hashHardwareForVerify) MUST stay byte-identical to
// setup.HashHardware — the provider uses the latter for register delta-detection
// and isannd uses it when it re-signs a relayed register frame (nlb_listener.go).
// If the two diverge, isannd-signed registers reconstruct a different
// RegisterDigest than RV verifies, so RV's ecrecover rejects every node. Guard
// the invariant so a future edit to either copy trips a test, not the live mesh.
func TestHashHardwareMatchesSetup(t *testing.T) {
	cases := []*setup.HardwareSpec{
		nil,
		{},
		{GPUs: []setup.GPUSpec{}},
		{RAM: &setup.RAMSpec{}},
	}
	for i, hw := range cases {
		if got, want := setup.HashHardware(hw), hashHardwareForVerify(hw); got != want {
			t.Errorf("case %d: setup.HashHardware=%q != hashHardwareForVerify=%q", i, got, want)
		}
	}
}
