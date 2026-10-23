package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Helpers for the settlement SLA monitoring tests (LG-185).

func draftPayoutWithRail(t *testing.T, l *Ledger, id string, amount int64, rail string) PayoutInstruction {
	t.Helper()
	p, dup, err := l.CreatePayout(PayoutInstruction{
		ID:           id,
		PayeeAccount: "saver",
		AmountCents:  amount,
		Destination:  "FPS:6222000012345678",
		Maker:        "alice",
		Rail:         rail,
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if dup {
		t.Fatal("duplicate = true on first create")
	}
	return p
}

func TestNormalizeRail(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"fednow", "fednow"},
		{"FedNow", "fednow"},
		{"  RTP ", "rtp"},
		{"SEPA", "sepa"},
		{"swift", "swift"},
		{"GPI", "gpi"},
		{"ACH", "ach"},
	} {
		got, err := normalizeRail(tc.in)
		if err != nil {
			t.Errorf("normalizeRail(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeRail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"zelle", "fedwire", "x"} {
		if _, err := normalizeRail(bad); !errors.Is(err, ErrInvalidRail) {
			t.Errorf("normalizeRail(%q) err = %v, want ErrInvalidRail", bad, err)
		}
	}
}

func TestSetSLAConfig(t *testing.T) {
	l := seedPayoutLedger(t)
	// Defaults match the rail promises.
	for rail, want := range map[string]time.Duration{
		RailFedNow: 20 * time.Second,
		RailRTP:    20 * time.Second,
		RailSEPA:   24 * time.Hour,
		RailSWIFT:  48 * time.Hour,
		RailGPI:    24 * time.Hour,
		RailACH:    48 * time.Hour, // LG-187: ACH joins the vocabulary.
	} {
		got, err := l.SLADuration(rail)
		if err != nil {
			t.Fatalf("SLADuration(%q): %v", rail, err)
		}
		if got != want {
			t.Errorf("default SLA(%q) = %v, want %v", rail, got, want)
		}
	}
	// An override replaces the default for watches opened after it.
	if err := l.SetSLAConfig("fednow", time.Hour); err != nil {
		t.Fatalf("SetSLAConfig: %v", err)
	}
	if got, _ := l.SLADuration("fednow"); got != time.Hour {
		t.Errorf("SLA(fednow) = %v, want 1h", got)
	}
	// Unknown rail, empty rail, and non-positive durations are
	// refused fail-closed.
	if err := l.SetSLAConfig("zelle", time.Hour); !errors.Is(err, ErrInvalidRail) {
		t.Errorf("SetSLAConfig(zelle) err = %v, want ErrInvalidRail", err)
	}
	if err := l.SetSLAConfig("", time.Hour); !errors.Is(err, ErrInvalidRail) {
		t.Errorf("SetSLAConfig(empty) err = %v, want ErrInvalidRail", err)
	}
	if err := l.SetSLAConfig("rtp", 0); !errors.Is(err, ErrInvalidSLADuration) {
		t.Errorf("SetSLAConfig(rtp, 0) err = %v, want ErrInvalidSLADuration", err)
	}
	if err := l.SetSLAConfig("rtp", -time.Second); !errors.Is(err, ErrInvalidSLADuration) {
		t.Errorf("SetSLAConfig(rtp, -1s) err = %v, want ErrInvalidSLADuration", err)
	}
}

func TestCreatePayout_RailValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	// A name outside the rail vocabulary is rejected before anything
	// is recorded.
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID: "po-rail-bad", PayeeAccount: "saver", AmountCents: 1000,
		Destination: "FPS:1", Maker: "alice", Rail: "zelle",
	}); !errors.Is(err, ErrInvalidRail) {
		t.Fatalf("CreatePayout(zelle) err = %v, want ErrInvalidRail", err)
	}
	if _, ok := l.GetPayout("po-rail-bad"); ok {
		t.Fatal("rejected payout was recorded")
	}
	// The rail is normalized to lowercase on the way in.
	p := draftPayoutWithRail(t, l, "po-rail-fn", 1000, "FedNow")
	if p.Rail != RailFedNow {
		t.Errorf("payout rail = %q, want %q", p.Rail, RailFedNow)
	}
}

func TestSubmitPayout_OpensSLAWatch(t *testing.T) {
	l := seedPayoutLedger(t)
	// No stated rail: the watch rides the gpi rail — every payout is
	// a cross-border initiation with a UETR (LG-165).
	draftPayout(t, l, "po-sla-1", 100_000)
	p := approveSubmitPayout(t, l, "po-sla-1", "bob")
	w, ok := l.SLAWatch("po-sla-1")
	if !ok {
		t.Fatal("no SLA watch opened on submit")
	}
	if w.Kind != SLAWatchKindPayout {
		t.Errorf("watch kind = %q, want payout", w.Kind)
	}
	if w.Rail != RailGPI {
		t.Errorf("watch rail = %q, want gpi", w.Rail)
	}
	if want := p.SubmittedAt.Add(24 * time.Hour); !w.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want submitted+24h", w.Deadline)
	}
	if !w.ClosedAt.IsZero() || w.Breached {
		t.Error("fresh watch must be open and unbreached")
	}
	// A stated rail is honored instead.
	draftPayoutWithRail(t, l, "po-sla-2", 100_000, "fednow")
	p2 := approveSubmitPayout(t, l, "po-sla-2", "bob")
	w2, ok := l.SLAWatch("po-sla-2")
	if !ok {
		t.Fatal("no SLA watch opened on submit (fednow)")
	}
	if w2.Rail != RailFedNow {
		t.Errorf("watch rail = %q, want fednow", w2.Rail)
	}
	if want := p2.SubmittedAt.Add(20 * time.Second); !w2.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want submitted+20s", w2.Deadline)
	}
	// Re-submitting is idempotent: the clock is not restarted.
	if _, err := l.SubmitPayout("po-sla-2"); err != nil {
		t.Fatalf("re-submit: %v", err)
	}
	w2b, _ := l.SLAWatch("po-sla-2")
	if !w2b.Deadline.Equal(w2.Deadline) {
		t.Error("re-submit restarted the SLA clock")
	}
}

func TestCheckSLAClocks_Breach(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	draftPayoutWithRail(t, l, "po-breach", 100_000, "fednow")
	approveSubmitPayout(t, l, "po-breach", "bob")

	w, _ := l.SLAWatch("po-breach")
	// Inside the SLA: nothing fires.
	if fired := l.CheckSLAClocks(w.Deadline); len(fired) != 0 {
		t.Fatalf("fired %d breaches at the deadline, want 0", len(fired))
	}
	// Past the deadline: exactly one breach fires.
	fired := l.CheckSLAClocks(w.Deadline.Add(time.Second))
	if len(fired) != 1 {
		t.Fatalf("fired %d breaches, want 1", len(fired))
	}
	b := fired[0]
	if b.ID != "po-breach" || b.Kind != SLAWatchKindPayout || b.Rail != RailFedNow {
		t.Errorf("breach = %+v, want po-breach/payout/fednow", b)
	}
	// The breach is sticky: it fires once, and the watch stays marked
	// even on later evaluations.
	if fired := l.CheckSLAClocks(w.Deadline.Add(time.Hour)); len(fired) != 0 {
		t.Fatalf("re-fired %d breaches, want 0", len(fired))
	}
	w, _ = l.SLAWatch("po-breach")
	if !w.Breached {
		t.Error("watch breached = false after firing")
	}
	if counts := l.SLABreachCounts(); counts[RailFedNow] != 1 {
		t.Errorf("breach counts = %v, want fednow:1", counts)
	}
	// The breach is in the hash-chained audit trail.
	events := readAuditOps(t, al)
	if countOp(events, "sla_breached") != 1 {
		t.Fatalf("sla_breached events = %d, want 1", countOp(events, "sla_breached"))
	}
	ev := findOp(events, "sla_breached")
	if ev.TraceID != "po-breach" {
		t.Errorf("sla_breached trace = %q, want po-breach", ev.TraceID)
	}
	if ev.Details["rail"] != RailFedNow || ev.Details["kind"] != SLAWatchKindPayout {
		t.Errorf("sla_breached details = %v, want rail/kind", ev.Details)
	}
}

func TestSettlePayout_ClosesSLAWatch(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayoutWithRail(t, l, "po-settle-sla", 100_000, "fednow")
	p := approveSubmitPayout(t, l, "po-settle-sla", "bob")
	w, _ := l.SLAWatch("po-settle-sla")
	// Settling closes the watch: a settled payment never breaches.
	if _, err := l.SettlePayout("po-settle-sla", statementLineForPayout(p, p.SubmittedAt), StatementMatchOptions{}); err != nil {
		t.Fatalf("SettlePayout: %v", err)
	}
	w, _ = l.SLAWatch("po-settle-sla")
	if w.ClosedAt.IsZero() || w.CloseReason != SLACloseSettled {
		t.Errorf("watch close = (%v,%q), want settled", w.ClosedAt, w.CloseReason)
	}
	if fired := l.CheckSLAClocks(w.Deadline.Add(24 * time.Hour)); len(fired) != 0 {
		t.Errorf("fired %d breaches on a settled watch, want 0", len(fired))
	}
}

func TestFailPayout_ClosesSLAWatch(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-fail-sla", 100_000)
	approveSubmitPayout(t, l, "po-fail-sla", "bob")
	if _, err := l.FailPayout("po-fail-sla", "beneficiary bank rejected"); err != nil {
		t.Fatalf("FailPayout: %v", err)
	}
	w, ok := l.SLAWatch("po-fail-sla")
	if !ok {
		t.Fatal("no watch for the failed payout")
	}
	if w.CloseReason != SLACloseFailed {
		t.Errorf("close reason = %q, want failed", w.CloseReason)
	}
	if fired := l.CheckSLAClocks(w.Deadline.Add(24 * time.Hour)); len(fired) != 0 {
		t.Errorf("fired %d breaches on a failed watch, want 0", len(fired))
	}
}

func TestGPIAdvance_UpdatesSLAClock(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-gpi-sla", 100_000)
	approveSubmitPayout(t, l, "po-gpi-sla", "bob")
	w, _ := l.SLAWatch("po-gpi-sla")
	// A non-terminal gpi advance refreshes the watch's progress
	// timestamp but does not move the deadline and does not close it.
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSP, "", "ops"); err != nil {
		t.Fatalf("AdvanceGPI: %v", err)
	}
	w2, _ := l.SLAWatch("po-gpi-sla")
	if !w2.ClosedAt.IsZero() {
		t.Error("ACSP advance closed the watch")
	}
	if !w2.Deadline.Equal(w.Deadline) {
		t.Error("gpi progress moved the SLA deadline")
	}
	if w2.LastProgressAt.Before(w.LastProgressAt) {
		t.Error("gpi progress did not refresh LastProgressAt")
	}
	// The terminal ACCP advance closes the watch as settled: even
	// past the deadline, no breach fires.
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACCP, "", "ops"); err != nil {
		t.Fatalf("AdvanceGPI ACCP: %v", err)
	}
	w3, _ := l.SLAWatch("po-gpi-sla")
	if w3.CloseReason != SLACloseSettled {
		t.Errorf("close reason = %q, want settled", w3.CloseReason)
	}
	if fired := l.CheckSLAClocks(w3.Deadline.Add(48 * time.Hour)); len(fired) != 0 {
		t.Errorf("fired %d breaches on a gpi-settled watch, want 0", len(fired))
	}
}

func TestGPIReject_ClosesSLAWatch(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-gpi-rjct", 100_000)
	approveSubmitPayout(t, l, "po-gpi-rjct", "bob")
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "AC03", "ops"); err != nil {
		t.Fatalf("AdvanceGPI RJCT: %v", err)
	}
	w, _ := l.SLAWatch("po-gpi-rjct")
	if w.CloseReason != SLACloseRejected {
		t.Errorf("close reason = %q, want rejected", w.CloseReason)
	}
	if fired := l.CheckSLAClocks(w.Deadline.Add(48 * time.Hour)); len(fired) != 0 {
		t.Errorf("fired %d breaches on a rejected watch, want 0", len(fired))
	}
	if counts := l.SLABreachCounts(); len(counts) != 0 {
		t.Errorf("breach counts = %v, want none", counts)
	}
}

func TestTransfer_OpensSLAWatch(t *testing.T) {
	l := New()
	seedBalance(t, l, "payer", 1_000_000)
	// A rail-bound transfer opens a watch at commit.
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-sla-1", From: "payer", To: "payee",
		AmountCents: 50_000, Currency: "USD", Rail: "sepa",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	w, ok := l.SLAWatch("tx-sla-1")
	if !ok {
		t.Fatal("no SLA watch opened on rail-bound transfer")
	}
	if w.Kind != SLAWatchKindTransfer || w.Rail != RailSEPA {
		t.Errorf("watch = kind %q rail %q, want transfer/sepa", w.Kind, w.Rail)
	}
	if want := w.OpenedAt.Add(24 * time.Hour); !w.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want opened+24h", w.Deadline)
	}
	fired := l.CheckSLAClocks(w.Deadline.Add(time.Second))
	if len(fired) != 1 || fired[0].Rail != RailSEPA || fired[0].Kind != SLAWatchKindTransfer {
		t.Fatalf("breach = %+v, want one sepa/transfer breach", fired)
	}
	// A transfer with no rail opens no watch.
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-sla-2", From: "payer", To: "payee",
		AmountCents: 10_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if _, ok := l.SLAWatch("tx-sla-2"); ok {
		t.Error("watch opened for a rail-less transfer")
	}
	// An unknown rail is rejected before anything books.
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-sla-3", From: "payer", To: "payee",
		AmountCents: 10_000, Currency: "USD", Rail: "zelle",
	}); !errors.Is(err, ErrInvalidRail) {
		t.Fatalf("PostTransfer(zelle) err = %v, want ErrInvalidRail", err)
	}
	// LG-187: ach is now a vocabulary rail, so a routed ACH transfer
	// opens an SLA watch with the 48h default.
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-sla-4", From: "payer", To: "payee",
		AmountCents: 10_000, Currency: "USD", Rail: "ach",
	}); err != nil {
		t.Fatalf("PostTransfer(ach): %v", err)
	}
	w4, ok4 := l.SLAWatch("tx-sla-4")
	if !ok4 {
		t.Fatal("no SLA watch opened for an ach transfer")
	}
	if w4.Rail != RailACH {
		t.Errorf("watch rail = %q, want ach", w4.Rail)
	}
}

func TestSLAConfig_AppliesToLaterWatches(t *testing.T) {
	l := seedPayoutLedger(t)
	if err := l.SetSLAConfig("fednow", 5*time.Minute); err != nil {
		t.Fatalf("SetSLAConfig: %v", err)
	}
	draftPayoutWithRail(t, l, "po-sla-cfg", 100_000, "fednow")
	p := approveSubmitPayout(t, l, "po-sla-cfg", "bob")
	w, _ := l.SLAWatch("po-sla-cfg")
	if want := p.SubmittedAt.Add(5 * time.Minute); !w.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want submitted+5m", w.Deadline)
	}
	// The override does not reach back into watches opened before it.
	if err := l.SetSLAConfig("fednow", time.Hour); err != nil {
		t.Fatalf("SetSLAConfig: %v", err)
	}
	w2, _ := l.SLAWatch("po-sla-cfg")
	if !w2.Deadline.Equal(w.Deadline) {
		t.Error("mid-flight SLA change rewrote an open watch's deadline")
	}
}

func TestSLASweeper(t *testing.T) {
	if sw := StartSLASweeper(nil, nil, 0, nil); sw != nil {
		t.Error("StartSLASweeper(0) = non-nil, want nil (disabled)")
	}
	l := seedPayoutLedger(t)
	// A millisecond SLA so the sweeper can breach it in real time.
	if err := l.SetSLAConfig("fednow", time.Millisecond); err != nil {
		t.Fatalf("SetSLAConfig: %v", err)
	}
	draftPayoutWithRail(t, l, "po-sla-sweep", 100_000, "fednow")
	approveSubmitPayout(t, l, "po-sla-sweep", "bob")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sw := StartSLASweeper(ctx, l, 5*time.Millisecond, nil)
	if sw == nil {
		t.Fatal("StartSLASweeper returned nil for a positive interval")
	}
	defer sw.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w, _ := l.SLAWatch("po-sla-sweep")
		if w.Breached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not fire the breach in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := l.SLABreachCounts()[RailFedNow]; got != 1 {
		t.Errorf("breach count = %d, want 1", got)
	}
	if sw.Ticks() == 0 {
		t.Error("sweeper ticks = 0")
	}
	if sw.BreachedTotal() != 1 {
		t.Errorf("sweeper breached total = %d, want 1", sw.BreachedTotal())
	}
}

// TestSLAConcurrent exercises the watch registry under concurrent
// submits and clock evaluations: -race must stay silent.
func TestSLAConcurrent(t *testing.T) {
	l := seedPayoutLedger(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("po-sla-race-%d", i)
			p, dup, err := l.CreatePayout(PayoutInstruction{
				ID: id, PayeeAccount: "saver", AmountCents: 1000,
				Destination: "FPS:1", Maker: "alice", Rail: "fednow",
			})
			if err != nil || dup {
				t.Errorf("CreatePayout: %v dup=%v", err, dup)
				return
			}
			if _, err := l.ApprovePayout(p.ID, "bob"); err != nil {
				t.Errorf("ApprovePayout: %v", err)
				return
			}
			if _, err := l.SubmitPayout(p.ID); err != nil {
				t.Errorf("SubmitPayout: %v", err)
				return
			}
			l.CheckSLAClocks(time.Now().UTC())
			l.SLABreachCounts()
			l.SLAWatches()
			l.SLAWatch(id)
		}(i)
	}
	wg.Wait()
	if n := len(l.SLAWatches()); n != 8 {
		t.Errorf("watches = %d, want 8", n)
	}
}

func TestSLAWatches_Sorted(t *testing.T) {
	l := seedPayoutLedger(t)
	for _, id := range []string{"po-z", "po-a", "po-m"} {
		draftPayoutWithRail(t, l, id, 1000, "fednow")
		approveSubmitPayout(t, l, id, "bob")
	}
	got := l.SLAWatches()
	if len(got) != 3 {
		t.Fatalf("watches = %d, want 3", len(got))
	}
	if got[0].ID != "po-a" || got[1].ID != "po-m" || got[2].ID != "po-z" {
		t.Errorf("watches not sorted by ID: %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}
