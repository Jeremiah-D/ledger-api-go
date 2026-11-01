package ledger

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestAmendInstallmentPlanPartiesValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendInstallmentPlanPayer("ip-unknown", "buyer-2"); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan (payer): err = %v, want ErrInstallmentNotFound", err)
	}
	if _, err := l.AmendInstallmentPlanPayee("ip-unknown", "merchant-2"); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan (payee): err = %v, want ErrInstallmentNotFound", err)
	}
	mustOpenInstallmentPlan(t, l, "ip-parties")

	// Terminal: completed. Sweeping all three monthly periods
	// completes the plan.
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep to completion: %v", err)
	}
	plan, _ := l.InstallmentPlan("ip-parties")
	if plan.Status != InstallmentCompleted {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
	if _, err := l.AmendInstallmentPlanPayer("ip-parties", "buyer-2"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("completed plan (payer): err = %v, want ErrInstallmentBadTransition", err)
	}
	if _, err := l.AmendInstallmentPlanPayee("ip-parties", "merchant-2"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("completed plan (payee): err = %v, want ErrInstallmentBadTransition", err)
	}

	// Terminal: settled_early.
	l2 := New()
	mustOpenInstallmentPlan(t, l2, "ip-parties")
	if _, err := l2.SettleInstallmentEarly("ip-parties", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("early settle: %v", err)
	}
	if _, err := l2.AmendInstallmentPlanPayer("ip-parties", "buyer-2"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("settled_early plan (payer): err = %v, want ErrInstallmentBadTransition", err)
	}
	if _, err := l2.AmendInstallmentPlanPayee("ip-parties", "merchant-2"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("settled_early plan (payee): err = %v, want ErrInstallmentBadTransition", err)
	}

	// Posted periods: amending a plan that has already journaled a
	// period would restate history, so it fails closed — and the
	// plan is left exactly as it was.
	l3 := New()
	mustOpenInstallmentPlan(t, l3, "ip-parties")
	if _, err := l3.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	orig3, _ := l3.InstallmentPlan("ip-parties")
	if orig3.PostedPeriods != 1 {
		t.Fatalf("posted periods = %d, want 1", orig3.PostedPeriods)
	}
	if _, err := l3.AmendInstallmentPlanPayer("ip-parties", "buyer-2"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("plan with posted periods (payer): err = %v, want ErrInstallmentBadTransition", err)
	}
	if _, err := l3.AmendInstallmentPlanPayee("ip-parties", "merchant-2"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("plan with posted periods (payee): err = %v, want ErrInstallmentBadTransition", err)
	}
	got, _ := l3.InstallmentPlan("ip-parties")
	if !reflect.DeepEqual(got, orig3) {
		t.Errorf("plan after posted-period rejection = %+v, want %+v (untouched)", got, orig3)
	}

	// Creation gates, reused: empty parties and same-account
	// parties fail, and every rejection leaves the original plan
	// exactly as it was. The same-account check reads the live
	// other party: the payer may not become the current payee, and
	// the payee may not become the current payer.
	l4 := New()
	orig4 := mustOpenInstallmentPlan(t, l4, "ip-parties")
	badPayers := []struct {
		newPayer AccountID
		want     error
	}{
		{"", ErrEmptyInstallmentPayer},
		{"merchant", ErrInstallmentSameAccount},
	}
	for _, tc := range badPayers {
		if _, err := l4.AmendInstallmentPlanPayer("ip-parties", tc.newPayer); !errors.Is(err, tc.want) {
			t.Errorf("payer %q: err = %v, want %v", tc.newPayer, err, tc.want)
		}
		got, _ := l4.InstallmentPlan("ip-parties")
		if !reflect.DeepEqual(got, orig4) {
			t.Errorf("plan after payer %q rejection = %+v, want %+v (untouched)", tc.newPayer, got, orig4)
		}
	}
	badPayees := []struct {
		newPayee AccountID
		want     error
	}{
		{"", ErrEmptyInstallmentPayee},
		{"buyer", ErrInstallmentSameAccount},
	}
	for _, tc := range badPayees {
		if _, err := l4.AmendInstallmentPlanPayee("ip-parties", tc.newPayee); !errors.Is(err, tc.want) {
			t.Errorf("payee %q: err = %v, want %v", tc.newPayee, err, tc.want)
		}
		got, _ := l4.InstallmentPlan("ip-parties")
		if !reflect.DeepEqual(got, orig4) {
			t.Errorf("plan after payee %q rejection = %+v, want %+v (untouched)", tc.newPayee, got, orig4)
		}
	}

	// Amending to the current party is a no-op, not a change.
	if got, err := l4.AmendInstallmentPlanPayer("ip-parties", "buyer"); err != nil || !reflect.DeepEqual(got, orig4) {
		t.Errorf("same-payer amendment = %+v, %v, want %+v, nil", got, err, orig4)
	}
	if got, err := l4.AmendInstallmentPlanPayee("ip-parties", "merchant"); err != nil || !reflect.DeepEqual(got, orig4) {
		t.Errorf("same-payee amendment = %+v, %v, want %+v, nil", got, err, orig4)
	}
}

func TestAmendInstallmentPlanPartiesSweepHonorsNewParties(t *testing.T) {
	// The new parties govern every period from the next sweep: the
	// sweep reads the live plan row (DebitAccount: plan.Payee,
	// CreditAccount: plan.Payer), so a payer move re-routes the
	// credit leg and a payee move re-routes the debit leg.
	l := New()
	origPayer := mustOpenInstallmentPlan(t, l, "ip-payer-sweep")
	origPayee := mustOpenInstallmentPlan(t, l, "ip-payee-sweep")

	amended, err := l.AmendInstallmentPlanPayer("ip-payer-sweep", "buyer-2")
	if err != nil {
		t.Fatalf("AmendInstallmentPlanPayer: %v", err)
	}
	if amended.Payer != "buyer-2" {
		t.Errorf("amended payer = %q, want buyer-2", amended.Payer)
	}
	// Only the payer moves: everything else is untouched.
	amended.Payer = origPayer.Payer
	if !reflect.DeepEqual(amended, origPayer) {
		t.Errorf("amended plan = %+v, want %+v except payer", amended, origPayer)
	}
	amended2, err := l.AmendInstallmentPlanPayee("ip-payee-sweep", "merchant-2")
	if err != nil {
		t.Fatalf("AmendInstallmentPlanPayee: %v", err)
	}
	if amended2.Payee != "merchant-2" {
		t.Errorf("amended payee = %q, want merchant-2", amended2.Payee)
	}
	amended2.Payee = origPayee.Payee
	if !reflect.DeepEqual(amended2, origPayee) {
		t.Errorf("amended plan = %+v, want %+v except payee", amended2, origPayee)
	}
	// The amendments themselves post nothing.
	if got := len(l.Entries()); got != 0 {
		t.Fatalf("entries after amendments = %d, want 0 (amendments post nothing)", got)
	}

	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	entries := l.Entries()
	if len(entries) != 6 {
		t.Fatalf("entries = %d, want 6 (3 periods x 2 plans)", len(entries))
	}
	// Entries() ranges a map, so order is nondeterministic: sort
	// by ID, then assert each row's legs from its plan ID
	// ("ip-payee-sweep" sorts before "ip-payer-sweep").
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.ID, "installment/ip-payer-sweep/"):
			if e.DebitAccount != "merchant" || e.CreditAccount != "buyer-2" {
				t.Errorf("payer-plan row %s = debit %q credit %q, want merchant/buyer-2", e.ID, e.DebitAccount, e.CreditAccount)
			}
		case strings.HasPrefix(e.ID, "installment/ip-payee-sweep/"):
			if e.DebitAccount != "merchant-2" || e.CreditAccount != "buyer" {
				t.Errorf("payee-plan row %s = debit %q credit %q, want merchant-2/buyer", e.ID, e.DebitAccount, e.CreditAccount)
			}
		default:
			t.Errorf("unexpected entry ID %s", e.ID)
		}
	}
	// Balances follow the new parties: the old buyer/merchant pair
	// keeps only the plan that still names them.
	if got := l.Balance("buyer-2"); got != -1000 {
		t.Errorf("buyer-2 balance = %d, want -1000", got)
	}
	if got := l.Balance("merchant-2"); got != 1000 {
		t.Errorf("merchant-2 balance = %d, want 1000", got)
	}
	if got := l.Balance("buyer"); got != -1000 {
		t.Errorf("buyer balance = %d, want -1000", got)
	}
	if got := l.Balance("merchant"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000", got)
	}
}

func TestAmendInstallmentPlanPartiesOverdue(t *testing.T) {
	// Overdue is a hold, not a terminal state: an overdue plan with
	// zero posted periods may be amended, and the new parties govern
	// the catch-up sweep.
	//
	// Payer move: freeze the payer before the first sweep so period
	// 1 fails and the plan goes overdue with nothing posted; move
	// the payer to an unfrozen account and the catch-up sweep posts
	// all three periods against the new payer.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-parties-overdue-payer")
	l.Freeze("buyer")
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	plan, _ := l.InstallmentPlan("ip-parties-overdue-payer")
	if plan.Status != InstallmentOverdue || plan.PostedPeriods != 0 {
		t.Fatalf("plan = %+v, want overdue with 0 posted periods", plan)
	}
	amended, err := l.AmendInstallmentPlanPayer("ip-parties-overdue-payer", "buyer-2")
	if err != nil {
		t.Fatalf("AmendInstallmentPlanPayer overdue: %v", err)
	}
	if amended.Payer != "buyer-2" || amended.Status != InstallmentOverdue {
		t.Errorf("amended overdue = payer %q status %q, want buyer-2 overdue", amended.Payer, amended.Status)
	}
	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catch-up sweep: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("catch-up sweep posted %d periods, want 3", len(res))
	}
	for _, e := range l.Entries() {
		if e.CreditAccount != "buyer-2" || e.DebitAccount != "merchant" {
			t.Errorf("catch-up row %s = debit %q credit %q, want merchant/buyer-2", e.ID, e.DebitAccount, e.CreditAccount)
		}
	}
	plan, _ = l.InstallmentPlan("ip-parties-overdue-payer")
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}

	// Payee move: same overdue setup, move the payee, unfreeze the
	// payer, and the catch-up sweep debits the new payee.
	l2 := New()
	mustOpenInstallmentPlan(t, l2, "ip-parties-overdue-payee")
	l2.Freeze("buyer")
	if _, err := l2.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	amended2, err := l2.AmendInstallmentPlanPayee("ip-parties-overdue-payee", "merchant-2")
	if err != nil {
		t.Fatalf("AmendInstallmentPlanPayee overdue: %v", err)
	}
	if amended2.Payee != "merchant-2" || amended2.Status != InstallmentOverdue {
		t.Errorf("amended overdue = payee %q status %q, want merchant-2 overdue", amended2.Payee, amended2.Status)
	}
	l2.Unfreeze("buyer")
	res, err = l2.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catch-up sweep: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("catch-up sweep posted %d periods, want 3", len(res))
	}
	for _, e := range l2.Entries() {
		if e.DebitAccount != "merchant-2" || e.CreditAccount != "buyer" {
			t.Errorf("catch-up row %s = debit %q credit %q, want merchant-2/buyer", e.ID, e.DebitAccount, e.CreditAccount)
		}
	}
}

func TestAmendInstallmentPlanPartiesAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-parties-audit")
	// No-op revisions emit nothing (same parties).
	if _, err := l.AmendInstallmentPlanPayer("ip-parties-audit", "buyer"); err != nil {
		t.Fatalf("no-op payer amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanPayee("ip-parties-audit", "merchant"); err != nil {
		t.Fatalf("no-op payee amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanPayer("ip-parties-audit", "buyer-2"); err != nil {
		t.Fatalf("AmendInstallmentPlanPayer: %v", err)
	}
	if _, err := l.AmendInstallmentPlanPayee("ip-parties-audit", "merchant-2"); err != nil {
		t.Fatalf("AmendInstallmentPlanPayee: %v", err)
	}
	if _, err := l.AmendInstallmentPlanPayer("ip-parties-audit", "buyer-3"); err != nil {
		t.Fatalf("AmendInstallmentPlanPayer second: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	payerEvents := byOp["installment_payer_amended"]
	if len(payerEvents) != 2 {
		t.Fatalf("payer amend events = %d, want 2 (two revisions; no-ops must not audit)", len(payerEvents))
	}
	if payerEvents[0].TraceID != "ip-parties-audit" || payerEvents[0].Actor != "AmendInstallmentPlanPayer" {
		t.Errorf("payer event trace/actor = %q/%q, want ip-parties-audit/AmendInstallmentPlanPayer", payerEvents[0].TraceID, payerEvents[0].Actor)
	}
	d := payerEvents[0].Details
	if d["plan_id"] != "ip-parties-audit" ||
		d["previous_payer"] != "buyer" ||
		d["payer"] != "buyer-2" ||
		d["posted_periods"] != float64(0) {
		t.Errorf("payer event details = %+v, want ip-parties-audit buyer->buyer-2 posted_periods 0", d)
	}
	d = payerEvents[1].Details
	if d["previous_payer"] != "buyer-2" ||
		d["payer"] != "buyer-3" ||
		d["posted_periods"] != float64(0) {
		t.Errorf("second payer event details = %+v, want buyer-2->buyer-3 posted_periods 0", d)
	}
	payeeEvents := byOp["installment_payee_amended"]
	if len(payeeEvents) != 1 {
		t.Fatalf("payee amend events = %d, want 1 (one revision; no-op must not audit)", len(payeeEvents))
	}
	if payeeEvents[0].TraceID != "ip-parties-audit" || payeeEvents[0].Actor != "AmendInstallmentPlanPayee" {
		t.Errorf("payee event trace/actor = %q/%q, want ip-parties-audit/AmendInstallmentPlanPayee", payeeEvents[0].TraceID, payeeEvents[0].Actor)
	}
	d = payeeEvents[0].Details
	if d["plan_id"] != "ip-parties-audit" ||
		d["previous_payee"] != "merchant" ||
		d["payee"] != "merchant-2" ||
		d["posted_periods"] != float64(0) {
		t.Errorf("payee event details = %+v, want ip-parties-audit merchant->merchant-2 posted_periods 0", d)
	}
}
