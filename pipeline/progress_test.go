package pipeline

import "testing"

func TestPhaseString(t *testing.T) {
	want := map[Phase]string{
		PhaseConnecting: "connecting",
		PhaseStreaming:  "streaming",
		PhaseFinalizing: "finalizing",
		PhaseDone:       "done",
		Phase(99):       "unknown",
	}
	for p, s := range want {
		if got := p.String(); got != s {
			t.Errorf("Phase(%d).String() = %q, want %q", p, got, s)
		}
	}
}
