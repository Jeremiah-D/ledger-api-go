package main

import (
	"testing"
	"time"
)

func TestHoldSweepInterval(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"unset disables", "", 0, false},
		{"valid duration", "30s", 30 * time.Second, true},
		{"sub-second valid", "250ms", 250 * time.Millisecond, true},
		{"garbage disables", "soon", 0, false},
		{"zero disables", "0s", 0, false},
		{"negative disables", "-5s", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value == "" {
				t.Setenv("LEDGER_HOLD_SWEEP_INTERVAL", "")
			} else {
				t.Setenv("LEDGER_HOLD_SWEEP_INTERVAL", tc.value)
			}
			// t.Setenv with "" leaves the variable set-but-empty, which
			// holdSweepInterval treats exactly like unset.
			got, ok := holdSweepInterval()
			if ok != tc.ok || got != tc.want {
				t.Errorf("holdSweepInterval() = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
