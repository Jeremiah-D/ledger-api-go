package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// previewMinuteSchedule builds an active 1-minute-interval schedule
// starting 30 seconds in the future: nominal firings land at +30s,
// +90s, +150s, ... relative to now — 60 of them inside a 1-hour
// horizon — so a small live cap is directly visible in the preview
// length.
func previewMinuteSchedule(id string) TransferSchedule {
	return TransferSchedule{
		ID:          id,
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Minute,
		NextRunAt:   time.Now().UTC().Add(30 * time.Second),
	}
}

func createMinutePreviewSchedule(t *testing.T, l *Ledger, id string) {
	t.Helper()
	fundAccount(t, l, "alice", 100000, "USD")
	createPreviewSchedule(t, l, previewMinuteSchedule(id))
}

// TestSetSchedulePreviewMaxFiringsInvalidRejected pins the
// fail-closed gate: a cap below 1 is rejected and the live cap is
// left untouched. Zero is rejected, not a "list nothing" state — a
// zero cap would make every preview return zero firings, which is
// switching the preview surface off, not a meaningful cap (along
// LG-336).
func TestSetSchedulePreviewMaxFiringsInvalidRejected(t *testing.T) {
	l := New()
	if got := l.SchedulePreviewMaxFirings(); got != defaultMaxPreviewFirings {
		t.Fatalf("default cap = %d, want %d", got, defaultMaxPreviewFirings)
	}
	for _, n := range []int{0, -1, -100} {
		prev, err := l.SetSchedulePreviewMaxFirings(n)
		if !errors.Is(err, ErrInvalidSchedulePreviewMaxFirings) {
			t.Fatalf("SetSchedulePreviewMaxFirings(%d) = (%d, %v), want ErrInvalidSchedulePreviewMaxFirings", n, prev, err)
		}
		if prev != defaultMaxPreviewFirings {
			t.Fatalf("SetSchedulePreviewMaxFirings(%d) prev = %d, want %d (live)", n, prev, defaultMaxPreviewFirings)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetSchedulePreviewMaxFiringsCtx(context.Background(), 0); !errors.Is(err, ErrInvalidSchedulePreviewMaxFirings) || prev != defaultMaxPreviewFirings {
		t.Fatalf("SetSchedulePreviewMaxFiringsCtx(0) = (%d, %v), want (%d, ErrInvalidSchedulePreviewMaxFirings)", prev, err, defaultMaxPreviewFirings)
	}
	if got := l.SchedulePreviewMaxFirings(); got != defaultMaxPreviewFirings {
		t.Fatalf("cap after rejected revisions = %d, want %d", got, defaultMaxPreviewFirings)
	}
}

// TestSetSchedulePreviewMaxFiringsSameValueNoop pins the no-op:
// revising to the current cap returns the live value with a nil
// error and writes no audit event.
func TestSetSchedulePreviewMaxFiringsSameValueNoop(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	prev, err := l.SetSchedulePreviewMaxFirings(defaultMaxPreviewFirings)
	if err != nil {
		t.Fatalf("SetSchedulePreviewMaxFirings(%d) same value: %v", defaultMaxPreviewFirings, err)
	}
	if prev != defaultMaxPreviewFirings {
		t.Fatalf("prev = %d, want %d", prev, defaultMaxPreviewFirings)
	}
	waitAuditWritten(t, al, 1) // the log's startup alert only
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, auditDir)["schedule_preview_firings_retuned"]); n != 0 {
		t.Fatalf("same-value revision wrote %d retune events, want 0", n)
	}
}

// TestSchedulePreviewMaxFiringsDefault pins the construction-time
// default: a fresh ledger caps each preview at 10000 firings.
func TestSchedulePreviewMaxFiringsDefault(t *testing.T) {
	l := New()
	if got := l.SchedulePreviewMaxFirings(); got != 10000 {
		t.Fatalf("default cap = %d, want 10000", got)
	}
}

// TestSchedulePreviewMaxFiringsTightenToTwo pins the enforcement:
// with the cap tightened to 2, a 1-minute-interval plan over a
// 1-hour horizon (60 nominal firings) lists only the first 2.
func TestSchedulePreviewMaxFiringsTightenToTwo(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-min-2")
	if _, err := l.SetSchedulePreviewMaxFirings(2); err != nil {
		t.Fatalf("SetSchedulePreviewMaxFirings(2): %v", err)
	}
	p, err := l.PreviewSchedule("sched-min-2", time.Hour)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Firings) != 2 {
		t.Fatalf("firings = %d, want 2", len(p.Firings))
	}
	if got := p.Firings[1].DueAt.Sub(p.Firings[0].DueAt); got != time.Minute {
		t.Errorf("firing gap = %v, want 1m", got)
	}
}

// TestSchedulePreviewMaxFiringsLoosenToFive pins the other
// direction: the same 1-minute plan over the same 1-hour horizon
// lists 5 firings once the cap is loosened to 5.
func TestSchedulePreviewMaxFiringsLoosenToFive(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-min-5")
	if _, err := l.SetSchedulePreviewMaxFirings(5); err != nil {
		t.Fatalf("SetSchedulePreviewMaxFirings(5): %v", err)
	}
	p, err := l.PreviewSchedule("sched-min-5", time.Hour)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Firings) != 5 {
		t.Fatalf("firings = %d, want 5", len(p.Firings))
	}
}

// TestSchedulePreviewMaxFiringsTightenTakesEffectOnNextPreview pins
// that a revision lands on the next PreviewSchedule with no restart
// and no stored state to trim: the same plan lists all 60 nominal
// firings at the default cap, then only 3 once tightened.
func TestSchedulePreviewMaxFiringsTightenTakesEffectOnNextPreview(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-min-next")
	p, err := l.PreviewSchedule("sched-min-next", time.Hour)
	if err != nil {
		t.Fatalf("preview at default: %v", err)
	}
	if len(p.Firings) != 60 {
		t.Fatalf("firings at default = %d, want 60", len(p.Firings))
	}
	if _, err := l.SetSchedulePreviewMaxFirings(3); err != nil {
		t.Fatalf("SetSchedulePreviewMaxFirings(3): %v", err)
	}
	p, err = l.PreviewSchedule("sched-min-next", time.Hour)
	if err != nil {
		t.Fatalf("preview after tighten: %v", err)
	}
	if len(p.Firings) != 3 {
		t.Fatalf("firings after tighten = %d, want 3", len(p.Firings))
	}
}

// TestSchedulePreviewMaxFiringsGetterFollowsLiveValue pins that the
// getter tracks the live value through repeated revisions in both
// directions, including a return to the default.
func TestSchedulePreviewMaxFiringsGetterFollowsLiveValue(t *testing.T) {
	l := New()
	for _, n := range []int{5, 2, 50000, 10000} {
		prev, err := l.SetSchedulePreviewMaxFirings(n)
		if err != nil {
			t.Fatalf("SetSchedulePreviewMaxFirings(%d): %v", n, err)
		}
		_ = prev
		if got := l.SchedulePreviewMaxFirings(); got != n {
			t.Fatalf("getter after revision to %d = %d", n, got)
		}
	}
}

// TestSetSchedulePreviewMaxFiringsAuditDetailsExact pins the audit
// payload: exactly the two integers, previous and current — no
// schedule content ever rides in a retune event. (Integers decode
// as float64 through the audit log's JSON round-trip, along the
// audit_log_max_bytes tests.)
func TestSetSchedulePreviewMaxFiringsAuditDetailsExact(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.SetSchedulePreviewMaxFirings(5); err != nil {
		t.Fatalf("SetSchedulePreviewMaxFirings(5): %v", err)
	}
	if _, err := l.SetSchedulePreviewMaxFirings(2); err != nil {
		t.Fatalf("SetSchedulePreviewMaxFirings(2): %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["schedule_preview_firings_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("schedule_preview_firings_retuned events = %d, want 2", len(retuned))
	}
	want := []map[string]any{
		{"previous_max_firings": float64(10000), "current_max_firings": float64(5)},
		{"previous_max_firings": float64(5), "current_max_firings": float64(2)},
	}
	for i, ev := range retuned {
		if len(ev.Details) != 2 {
			t.Fatalf("retune[%d] details keys = %v, want exactly previous_max_firings + current_max_firings", i, ev.Details)
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

// TestSetSchedulePreviewMaxFiringsCtxActorAttribution pins the
// attribution on the Ctx variant: the operator identity replaces
// the default actor, while the plain setter keeps the "system"
// default.
func TestSetSchedulePreviewMaxFiringsCtxActorAttribution(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-preview")
	if prev, err := l.SetSchedulePreviewMaxFiringsCtx(ctx, 5); err != nil || prev != defaultMaxPreviewFirings {
		t.Fatalf("SetSchedulePreviewMaxFiringsCtx(5) = (%d, %v), want (10000, nil)", prev, err)
	}
	if prev, err := l.SetSchedulePreviewMaxFirings(2); err != nil || prev != 5 {
		t.Fatalf("SetSchedulePreviewMaxFirings(2) = (%d, %v), want (5, nil)", prev, err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["schedule_preview_firings_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("schedule_preview_firings_retuned events = %d, want 2", len(retuned))
	}
	if retuned[0].Actor != "ops-preview" {
		t.Errorf("Ctx retune actor = %q, want ops-preview", retuned[0].Actor)
	}
	if retuned[1].Actor != "system" {
		t.Errorf("plain retune actor = %q, want system", retuned[1].Actor)
	}
}

// TestSchedulePreviewMaxFiringsConcurrentRevisionAndPreview pins
// that concurrent cap revisions and concurrent previews never race
// and never over-list: every preview is bounded by the cap it read,
// and the final live cap is one of the revised values.
func TestSchedulePreviewMaxFiringsConcurrentRevisionAndPreview(t *testing.T) {
	l := New()
	createMinutePreviewSchedule(t, l, "sched-min-conc")
	caps := []int{1, 2, 5, 10000}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_, _ = l.SetSchedulePreviewMaxFirings(caps[(g+i)%len(caps)])
			}
		}(g)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				p, err := l.PreviewSchedule("sched-min-conc", time.Hour)
				if err != nil {
					t.Errorf("preview: %v", err)
					return
				}
				if len(p.Firings) > 60 {
					t.Errorf("preview listed %d firings, want at most 60 nominal in 1h", len(p.Firings))
					return
				}
			}
		}()
	}
	wg.Wait()
	got := l.SchedulePreviewMaxFirings()
	for _, c := range caps {
		if got == c {
			// With revisions settled, a final preview is bounded
			// by the live cap.
			p, err := l.PreviewSchedule("sched-min-conc", time.Hour)
			if err != nil {
				t.Fatalf("final preview: %v", err)
			}
			if len(p.Firings) > got {
				t.Errorf("final preview = %d firings, exceeds live cap %d", len(p.Firings), got)
			}
			return
		}
	}
	t.Errorf("live cap = %d, want one of %v", got, caps)
}
