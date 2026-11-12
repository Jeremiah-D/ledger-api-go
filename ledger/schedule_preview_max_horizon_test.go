package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestSetSchedulePreviewMaxHorizonInvalidRejected pins the
// fail-closed gate: a cap of zero or below is rejected with
// ErrScheduleBadHorizon and the live cap is left untouched. Zero
// is rejected, not a "preview nothing" state — a zero cap would
// make every preview reject, which is switching the preview
// surface off, not a meaningful cap (along LG-336).
func TestSetSchedulePreviewMaxHorizonInvalidRejected(t *testing.T) {
	l := New()
	if got := l.SchedulePreviewMaxHorizon(); got != defaultMaxSchedulePreviewHorizon {
		t.Fatalf("default cap = %v, want %v", got, defaultMaxSchedulePreviewHorizon)
	}
	for _, d := range []time.Duration{0, -time.Hour, -8760 * time.Hour} {
		prev, err := l.SetSchedulePreviewMaxHorizon(d)
		if !errors.Is(err, ErrScheduleBadHorizon) {
			t.Fatalf("SetSchedulePreviewMaxHorizon(%v) = (%v, %v), want ErrScheduleBadHorizon", d, prev, err)
		}
		if prev != defaultMaxSchedulePreviewHorizon {
			t.Fatalf("SetSchedulePreviewMaxHorizon(%v) prev = %v, want %v (live)", d, prev, defaultMaxSchedulePreviewHorizon)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetSchedulePreviewMaxHorizonCtx(context.Background(), 0); !errors.Is(err, ErrScheduleBadHorizon) || prev != defaultMaxSchedulePreviewHorizon {
		t.Fatalf("SetSchedulePreviewMaxHorizonCtx(0) = (%v, %v), want (%v, ErrScheduleBadHorizon)", prev, err, defaultMaxSchedulePreviewHorizon)
	}
	if got := l.SchedulePreviewMaxHorizon(); got != defaultMaxSchedulePreviewHorizon {
		t.Fatalf("cap after rejected revisions = %v, want %v", got, defaultMaxSchedulePreviewHorizon)
	}
}

// TestSetSchedulePreviewMaxHorizonSameValueNoop pins the no-op:
// revising to the current cap returns the live value with a nil
// error and writes no audit event.
func TestSetSchedulePreviewMaxHorizonSameValueNoop(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	prev, err := l.SetSchedulePreviewMaxHorizon(defaultMaxSchedulePreviewHorizon)
	if err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(%v) same value: %v", defaultMaxSchedulePreviewHorizon, err)
	}
	if prev != defaultMaxSchedulePreviewHorizon {
		t.Fatalf("prev = %v, want %v", prev, defaultMaxSchedulePreviewHorizon)
	}
	waitAuditWritten(t, al, 1) // the log's startup alert only
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, auditDir)["schedule_preview_horizon_retuned"]); n != 0 {
		t.Fatalf("same-value revision wrote %d retune events, want 0", n)
	}
}

// TestSchedulePreviewMaxHorizonDefault pins the construction-time
// default: a fresh ledger caps each preview window at 8760h.
func TestSchedulePreviewMaxHorizonDefault(t *testing.T) {
	l := New()
	if got := l.SchedulePreviewMaxHorizon(); got != 8760*time.Hour {
		t.Fatalf("default cap = %v, want 8760h", got)
	}
}

// TestSchedulePreviewMaxHorizonTightenTo48h pins the enforcement:
// with the cap tightened to 48h, a 72h horizon is rejected with
// ErrScheduleBadHorizon while a 24h horizon still previews.
func TestSchedulePreviewMaxHorizonTightenTo48h(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-hz-48")
	if _, err := l.SetSchedulePreviewMaxHorizon(48 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(48h): %v", err)
	}
	if _, err := l.PreviewSchedule("sched-hz-48", 72*time.Hour); !errors.Is(err, ErrScheduleBadHorizon) {
		t.Fatalf("preview 72h after tighten: err = %v, want ErrScheduleBadHorizon", err)
	}
	p, err := l.PreviewSchedule("sched-hz-48", 24*time.Hour)
	if err != nil {
		t.Fatalf("preview 24h after tighten: %v", err)
	}
	if len(p.Firings) != 1440 {
		t.Fatalf("firings = %d, want 1440 (1-minute cadence over 24h)", len(p.Firings))
	}
}

// TestSchedulePreviewMaxHorizonLoosenTo17520h pins the other
// direction and the orthogonality with LG-398: with the cap
// loosened to 17520h (two years), a 10000h horizon is accepted,
// and the firing list is still truncated by the per-preview
// firing cap (10000) even though the window holds far more
// nominal firings (a 1-minute cadence over 10000h is 600000).
func TestSchedulePreviewMaxHorizonLoosenTo17520h(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-hz-17520")
	if _, err := l.SetSchedulePreviewMaxHorizon(17520 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(17520h): %v", err)
	}
	p, err := l.PreviewSchedule("sched-hz-17520", 10000*time.Hour)
	if err != nil {
		t.Fatalf("preview 10000h after loosen: %v", err)
	}
	if len(p.Firings) != defaultMaxPreviewFirings {
		t.Fatalf("firings = %d, want %d (firing cap still truncates)", len(p.Firings), defaultMaxPreviewFirings)
	}
}

// TestSchedulePreviewMaxHorizonTightenTakesEffectOnNextPreview
// pins that a revision lands on the next PreviewSchedule with no
// restart and no stored state to trim: the same plan previews a
// 72h horizon at the default cap, rejects it once tightened to
// 48h, and accepts it again once loosened past it.
func TestSchedulePreviewMaxHorizonTightenTakesEffectOnNextPreview(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-hz-next")
	p, err := l.PreviewSchedule("sched-hz-next", 72*time.Hour)
	if err != nil {
		t.Fatalf("preview at default: %v", err)
	}
	if len(p.Firings) != 4320 {
		t.Fatalf("firings at default = %d, want 4320 (1-minute cadence over 72h)", len(p.Firings))
	}
	if _, err := l.SetSchedulePreviewMaxHorizon(48 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(48h): %v", err)
	}
	if _, err := l.PreviewSchedule("sched-hz-next", 72*time.Hour); !errors.Is(err, ErrScheduleBadHorizon) {
		t.Fatalf("preview 72h after tighten: err = %v, want ErrScheduleBadHorizon", err)
	}
	if _, err := l.SetSchedulePreviewMaxHorizon(17520 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(17520h): %v", err)
	}
	p, err = l.PreviewSchedule("sched-hz-next", 72*time.Hour)
	if err != nil {
		t.Fatalf("preview after loosen: %v", err)
	}
	if len(p.Firings) != 4320 {
		t.Fatalf("firings after loosen = %d, want 4320", len(p.Firings))
	}
}

// TestSchedulePreviewMaxHorizonGetterFollowsLiveValue pins that
// the getter tracks the live value through repeated revisions in
// both directions, including a return to the default.
func TestSchedulePreviewMaxHorizonGetterFollowsLiveValue(t *testing.T) {
	l := New()
	for _, d := range []time.Duration{48 * time.Hour, 24 * time.Hour, 17520 * time.Hour, 8760 * time.Hour} {
		prev, err := l.SetSchedulePreviewMaxHorizon(d)
		if err != nil {
			t.Fatalf("SetSchedulePreviewMaxHorizon(%v): %v", d, err)
		}
		_ = prev
		if got := l.SchedulePreviewMaxHorizon(); got != d {
			t.Fatalf("getter after revision to %v = %v", d, got)
		}
	}
}

// TestSetSchedulePreviewMaxHorizonAuditDetailsExact pins the audit
// payload: exactly the two duration strings, previous and current
// — no schedule content ever rides in a retune event. (Duration
// strings round-trip as plain strings through the audit log's JSON.)
func TestSetSchedulePreviewMaxHorizonAuditDetailsExact(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.SetSchedulePreviewMaxHorizon(48 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(48h): %v", err)
	}
	if _, err := l.SetSchedulePreviewMaxHorizon(17520 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(17520h): %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["schedule_preview_horizon_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("schedule_preview_horizon_retuned events = %d, want 2", len(retuned))
	}
	want := []map[string]any{
		{"previous_max_horizon": "8760h0m0s", "current_max_horizon": "48h0m0s"},
		{"previous_max_horizon": "48h0m0s", "current_max_horizon": "17520h0m0s"},
	}
	for i, ev := range retuned {
		if len(ev.Details) != 2 {
			t.Fatalf("retune[%d] details keys = %v, want exactly previous_max_horizon + current_max_horizon", i, ev.Details)
		}
		for k, v := range want[i] {
			if ev.Details[k] != v {
				t.Errorf("retune[%d] details[%q] = %v, want %v", i, k, ev.Details[k], v)
			}
		}
		if ev.Actor != "system" {
			t.Errorf("retune[%d] actor = %q, want system (plain setter default)", i, ev.Actor)
		}
	}
}

// TestSetSchedulePreviewMaxHorizonCtxActorAttribution pins the
// attribution on the Ctx variant: the operator identity replaces
// the default actor, while the plain setter keeps the "system"
// default.
func TestSetSchedulePreviewMaxHorizonCtxActorAttribution(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-horizon")
	if prev, err := l.SetSchedulePreviewMaxHorizonCtx(ctx, 48*time.Hour); err != nil || prev != defaultMaxSchedulePreviewHorizon {
		t.Fatalf("SetSchedulePreviewMaxHorizonCtx(48h) = (%v, %v), want (8760h, nil)", prev, err)
	}
	if prev, err := l.SetSchedulePreviewMaxHorizon(17520 * time.Hour); err != nil || prev != 48*time.Hour {
		t.Fatalf("SetSchedulePreviewMaxHorizon(17520h) = (%v, %v), want (48h, nil)", prev, err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["schedule_preview_horizon_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("schedule_preview_horizon_retuned events = %d, want 2", len(retuned))
	}
	if retuned[0].Actor != "ops-horizon" {
		t.Errorf("Ctx retune actor = %q, want ops-horizon", retuned[0].Actor)
	}
	if retuned[1].Actor != "system" {
		t.Errorf("plain retune actor = %q, want system", retuned[1].Actor)
	}
}

// TestSchedulePreviewMaxHorizonConcurrentRevisionAndPreview pins
// that concurrent cap revisions and concurrent previews never race
// and never accept a horizon beyond the cap in force: every
// preview either succeeds inside the cap it read or fails with
// ErrScheduleBadHorizon, and the final live cap is one of the
// revised values.
func TestSchedulePreviewMaxHorizonConcurrentRevisionAndPreview(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-hz-conc")
	caps := []time.Duration{24 * time.Hour, 48 * time.Hour, 72 * time.Hour, 8760 * time.Hour}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_, _ = l.SetSchedulePreviewMaxHorizon(caps[(g+i)%len(caps)])
			}
		}(g)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				p, err := l.PreviewSchedule("sched-hz-conc", 72*time.Hour)
				if err != nil {
					if !errors.Is(err, ErrScheduleBadHorizon) {
						t.Errorf("preview: %v, want nil or ErrScheduleBadHorizon", err)
					}
					continue
				}
				if len(p.Firings) > 4320 {
					t.Errorf("preview listed %d firings, want at most 4320 nominal in 72h", len(p.Firings))
					return
				}
			}
		}()
	}
	wg.Wait()
	got := l.SchedulePreviewMaxHorizon()
	for _, c := range caps {
		if got == c {
			return
		}
	}
	t.Errorf("live cap = %v, want one of %v", got, caps)
}
