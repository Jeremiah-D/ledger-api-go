package main

import (
	"testing"
	"time"
)

func TestFXRateWatchConfig(t *testing.T) {
	cases := []struct {
		name      string
		interval  string
		threshold string
		wantInt   time.Duration
		wantThr   time.Duration
		ok        bool
	}{
		{"unset disables", "", "", 0, 0, false},
		{"interval only", "5m", "", 5 * time.Minute, 0, true},
		{"interval plus threshold", "5m", "72h", 5 * time.Minute, 72 * time.Hour, true},
		{"garbage interval disables", "soon", "72h", 0, 0, false},
		{"zero interval disables", "0s", "72h", 0, 0, false},
		{"negative interval disables", "-5s", "72h", 0, 0, false},
		// The threshold is opt-in: a bad threshold only disables stale
		// warnings, never the watcher itself.
		{"garbage threshold disables stale only", "5m", "soon", 5 * time.Minute, 0, true},
		{"zero threshold disables stale only", "5m", "0s", 5 * time.Minute, 0, true},
		{"negative threshold disables stale only", "5m", "-1h", 5 * time.Minute, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LEDGER_FX_RATE_WATCH_INTERVAL", tc.interval)
			t.Setenv("LEDGER_FX_RATE_WARNING_THRESHOLD", tc.threshold)
			gotInt, gotThr, ok := fxRateWatchConfig()
			if ok != tc.ok || gotInt != tc.wantInt || gotThr != tc.wantThr {
				t.Errorf("fxRateWatchConfig() = (%v, %v, %v), want (%v, %v, %v)",
					gotInt, gotThr, ok, tc.wantInt, tc.wantThr, tc.ok)
			}
		})
	}
}
