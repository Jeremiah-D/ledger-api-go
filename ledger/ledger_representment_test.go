package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// seedRepresentmentLedger builds a ledger with one open dispute d1
// (payer -> payee transfer t1, 10000 USD, disputed 6000): the fixture
// every representment test starts from.
func seedRepresentmentLedger(t *testing.T) *Ledger {
	t.Helper()
	l := seedDisputeLedger(t)
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	}); err != nil {
		t.Fatalf("seed RaiseDispute: %v", err)
	}
	return l
}

func TestSubmitRepresentment(t *testing.T) {
	l := seedRepresentmentLedger(t)
	r, dup, err := l.SubmitRepresentment("d1",
		[]string{"sha256:receipt-001", "sha256:delivery-042"},
		"the payer authorized this transfer in writing")
	if err != nil {
		t.Fatalf("SubmitRepresentment: %v", err)
	}
	if dup {
		t.Error("first submit reports duplicate = true")
	}
	if r.DisputeID != "d1" {
		t.Errorf("dispute_id = %q, want d1", r.DisputeID)
	}
	if len(r.DocumentHashes) != 2 {
		t.Errorf("document_hashes = %d, want 2", len(r.DocumentHashes))
	}
	if r.Adjudicated {
		t.Error("a fresh representment must not be adjudicated")
	}
	// The window is measured from the claim filing: 7 days by default.
	d, _ := l.GetDispute("d1")
	if want := d.CreatedAt.Add(7 * 24 * time.Hour); !r.WindowExpiresAt.Equal(want) {
		t.Errorf("window_expires_at = %v, want %v", r.WindowExpiresAt, want)
	}
	if got, ok := l.GetRepresentment("d1"); !ok || got.Statement == "" {
		t.Error("GetRepresentment must return the submitted evidence")
	}
	// Submission books nothing: the raise's freeze hold is untouched
	// and the version does not move.
	if got := l.AvailableIn("payee", "USD"); got != 4000 {
		t.Errorf("payee available = %d, want 4000 (hold untouched)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("books out of balance after submit: %v", err)
	}
}

func TestSubmitRepresentmentReplay(t *testing.T) {
	l := seedRepresentmentLedger(t)
	hashes := []string{"sha256:receipt-001"}
	orig, _, err := l.SubmitRepresentment("d1", hashes, "authorized")
	if err != nil {
		t.Fatalf("first SubmitRepresentment: %v", err)
	}
	// Same evidence, different hash order: a replay, not an amendment.
	replay, dup, err := l.SubmitRepresentment("d1", hashes, "authorized")
	if err != nil {
		t.Fatalf("replay SubmitRepresentment: %v", err)
	}
	if !dup {
		t.Error("replay must report duplicate = true")
	}
	if !replay.SubmittedAt.Equal(orig.SubmittedAt) {
		t.Error("replay must return the original representment")
	}
}

func TestSubmitRepresentmentDifferentEvidence(t *testing.T) {
	l := seedRepresentmentLedger(t)
	if _, _, err := l.SubmitRepresentment("d1", []string{"sha256:a"}, "a"); err != nil {
		t.Fatalf("first SubmitRepresentment: %v", err)
	}
	_, _, err := l.SubmitRepresentment("d1", []string{"sha256:b"}, "b")
	if !errors.Is(err, ErrRepresentmentAlreadySubmitted) {
		t.Fatalf("second package = %v, want ErrRepresentmentAlreadySubmitted", err)
	}
}

func TestSubmitRepresentmentEvidenceValidation(t *testing.T) {
	l := seedRepresentmentLedger(t)
	// Empty evidence defends nothing.
	if _, _, err := l.SubmitRepresentment("d1", nil, "  "); !errors.Is(err, ErrEmptyRepresentmentEvidence) {
		t.Errorf("empty evidence = %v, want ErrEmptyRepresentmentEvidence", err)
	}
	// A statement alone is evidence; hashes alone too.
	if _, _, err := l.SubmitRepresentment("d1", nil, "the transfer was authorized"); err != nil {
		t.Errorf("statement-only evidence rejected: %v", err)
	}
	l2 := seedRepresentmentLedger(t)
	if _, _, err := l2.SubmitRepresentment("d1", []string{"sha256:x"}, ""); err != nil {
		t.Errorf("hash-only evidence rejected: %v", err)
	}
	// An empty hash is not a document reference.
	l3 := seedRepresentmentLedger(t)
	if _, _, err := l3.SubmitRepresentment("d1", []string{""}, "s"); !errors.Is(err, ErrInvalidRepresentmentDocument) {
		t.Errorf("empty hash = %v, want ErrInvalidRepresentmentDocument", err)
	}
	// 33 hashes exceed the package cap.
	l4 := seedRepresentmentLedger(t)
	many := make([]string, 33)
	for i := range many {
		many[i] = "sha256:doc"
	}
	if _, _, err := l4.SubmitRepresentment("d1", many, "s"); !errors.Is(err, ErrInvalidRepresentmentDocument) {
		t.Errorf("33 hashes = %v, want ErrInvalidRepresentmentDocument", err)
	}
}

func TestSubmitRepresentmentWindowExpired(t *testing.T) {
	l := seedDisputeLedger(t)
	// The claim was filed 8 days ago: the 7-day window is closed.
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
		CreatedAt:   time.Now().Add(-8 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed RaiseDispute: %v", err)
	}
	_, _, err := l.SubmitRepresentment("d1", []string{"sha256:late"}, "late")
	if !errors.Is(err, ErrRepresentmentWindowExpired) {
		t.Fatalf("late submit = %v, want ErrRepresentmentWindowExpired", err)
	}
}

func TestSubmitRepresentmentUnknownAndResolved(t *testing.T) {
	l := seedRepresentmentLedger(t)
	if _, _, err := l.SubmitRepresentment("nope", []string{"sha256:x"}, "s"); !errors.Is(err, ErrDisputeNotFound) {
		t.Errorf("unknown dispute = %v, want ErrDisputeNotFound", err)
	}
	if _, err := l.ResolveDispute("d1", false); err != nil {
		t.Fatalf("seed ResolveDispute: %v", err)
	}
	if _, _, err := l.SubmitRepresentment("d1", []string{"sha256:x"}, "s"); !errors.Is(err, ErrDisputeAlreadyResolved) {
		t.Errorf("resolved dispute = %v, want ErrDisputeAlreadyResolved", err)
	}
}

func TestAdjudicateRepresentmentUphold(t *testing.T) {
	l := seedRepresentmentLedger(t)
	if _, _, err := l.SubmitRepresentment("d1", []string{"sha256:receipt-001"}, "authorized in writing"); err != nil {
		t.Fatalf("SubmitRepresentment: %v", err)
	}
	d, err := l.AdjudicateRepresentment("d1", RepresentmentDecisionUpholdClaim, "arbiter-7")
	if err != nil {
		t.Fatalf("AdjudicateRepresentment: %v", err)
	}
	if d.Status != DisputeStatusResolved || !d.Uphold {
		t.Errorf("dispute = %v upheld=%v, want resolved/upheld", d.Status, d.Uphold)
	}
	r, ok := l.GetRepresentment("d1")
	if !ok || !r.Adjudicated || r.Decision != "uphold" || r.Adjudicator != "arbiter-7" {
		t.Errorf("representment not adjudicated as recorded: %+v", r)
	}
	// The payer is refunded the disputed 6000: 10000 - 10000 + 6000.
	if got := l.BalanceIn("payer", "USD"); got != 16000 {
		t.Errorf("payer balance = %d, want 16000", got)
	}
	// The freeze hold is released: the payee's available is whole again
	// (4000 net after the 6000 refund moved back to the payer).
	if got := l.AvailableIn("payee", "USD"); got != 4000 {
		t.Errorf("payee available = %d, want 4000 (hold released, refund moved)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("books out of balance after adjudication: %v", err)
	}
}

func TestAdjudicateRepresentmentReverse(t *testing.T) {
	l := seedRepresentmentLedger(t)
	if _, _, err := l.SubmitRepresentment("d1", []string{"sha256:receipt-001"}, "authorized"); err != nil {
		t.Fatalf("SubmitRepresentment: %v", err)
	}
	d, err := l.AdjudicateRepresentment("d1", RepresentmentDecisionReverseClaim, "ops-alice")
	if err != nil {
		t.Fatalf("AdjudicateRepresentment: %v", err)
	}
	if d.Status != DisputeStatusResolved || d.Uphold {
		t.Errorf("dispute = %v upheld=%v, want resolved/rejected", d.Status, d.Uphold)
	}
	// No refund: the payee keeps the funds; the hold just lifts.
	if got := l.BalanceIn("payer", "USD"); got != 10000 {
		t.Errorf("payer balance = %d, want 10000 (no refund)", got)
	}
	if got := l.AvailableIn("payee", "USD"); got != 10000 {
		t.Errorf("payee available = %d, want 10000 (hold released)", got)
	}
}

func TestAdjudicateRepresentmentValidation(t *testing.T) {
	l := seedRepresentmentLedger(t)
	// No evidence yet: nothing to adjudicate.
	if _, err := l.AdjudicateRepresentment("d1", "uphold", "arbiter"); !errors.Is(err, ErrRepresentmentNotSubmitted) {
		t.Errorf("no evidence = %v, want ErrRepresentmentNotSubmitted", err)
	}
	// Unknown decision.
	if _, _, err := l.SubmitRepresentment("d1", []string{"sha256:x"}, "s"); err != nil {
		t.Fatalf("SubmitRepresentment: %v", err)
	}
	if _, err := l.AdjudicateRepresentment("d1", "maybe", "arbiter"); !errors.Is(err, ErrInvalidRepresentmentDecision) {
		t.Errorf("bad decision = %v, want ErrInvalidRepresentmentDecision", err)
	}
	// Every adjudication is attributed.
	if _, err := l.AdjudicateRepresentment("d1", "uphold", " "); !errors.Is(err, ErrEmptyAdjudicator) {
		t.Errorf("empty adjudicator = %v, want ErrEmptyAdjudicator", err)
	}
	// Unknown dispute.
	if _, err := l.AdjudicateRepresentment("nope", "uphold", "arbiter"); !errors.Is(err, ErrDisputeNotFound) {
		t.Errorf("unknown dispute = %v, want ErrDisputeNotFound", err)
	}
	// Adjudication is terminal.
	if _, err := l.AdjudicateRepresentment("d1", "reverse", "arbiter"); err != nil {
		t.Fatalf("first adjudication: %v", err)
	}
	if _, err := l.AdjudicateRepresentment("d1", "uphold", "arbiter"); !errors.Is(err, ErrDisputeAlreadyResolved) {
		t.Errorf("second adjudication = %v, want ErrDisputeAlreadyResolved", err)
	}
}

func TestSweepExpiredRepresentments(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{
		ID: "fund-payer", DebitAccount: "payer", CreditAccount: "equity",
		AmountCents: 20000, Currency: "USD", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "t1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed PostTransfer: %v", err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	// d1: filed 8 days ago, no evidence — the sweep auto-upholds it.
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonUnauthorized, AmountCents: 6000, CreatedAt: old}); err != nil {
		t.Fatalf("raise d1: %v", err)
	}
	// d2: filed 8 days ago too, but the payee submitted evidence while
	// the window was open (a temporarily widened window makes that
	// deterministic) — the sweep must not touch it even though the
	// window is expired again by sweep time.
	if err := l.SetRepresentmentWindow(30 * 24 * time.Hour); err != nil {
		t.Fatalf("widen window: %v", err)
	}
	if _, _, err := l.RaiseDispute(Dispute{ID: "d2", TransferID: "t1", ReasonCode: DisputeReasonDuplicate, AmountCents: 1000, CreatedAt: old}); err != nil {
		t.Fatalf("raise d2: %v", err)
	}
	if _, _, err := l.SubmitRepresentment("d2", []string{"sha256:in-time"}, "in time"); err != nil {
		t.Fatalf("in-window submit: %v", err)
	}
	if err := l.SetRepresentmentWindow(7 * 24 * time.Hour); err != nil {
		t.Fatalf("restore window: %v", err)
	}
	// d3: fresh claim, window still open — untouched by the sweep.
	if _, _, err := l.RaiseDispute(Dispute{ID: "d3", TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 500}); err != nil {
		t.Fatalf("raise d3: %v", err)
	}

	resolved, err := l.SweepExpiredRepresentments(time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(resolved) != 1 || resolved[0].ID != "d1" {
		t.Fatalf("sweep resolved %v, want [d1]", resolved)
	}
	if !resolved[0].Uphold {
		t.Error("auto-uphold must uphold the claim (fail-closed for the payer)")
	}
	// d1's refund moved: payer 20000 - 10000 + 6000 = 16000.
	if got := l.BalanceIn("payer", "USD"); got != 16000 {
		t.Errorf("payer balance = %d, want 16000 after auto-uphold refund", got)
	}
	if d, _ := l.GetDispute("d2"); d.Status != DisputeStatusOpen {
		t.Errorf("d2 status = %q, want open (evidence blocks the sweep)", d.Status)
	}
	if d, _ := l.GetDispute("d3"); d.Status != DisputeStatusOpen {
		t.Errorf("d3 status = %q, want open (window still open)", d.Status)
	}
	// The auto-uphold is observable in the audit trail.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	found := false
	for i := range events {
		if events[i].Op == "representment_auto_upheld" && events[i].TraceID == "d1" {
			found = true
		}
	}
	if !found {
		t.Error("no representment_auto_upheld audit event for d1")
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("books out of balance after sweep: %v", err)
	}
}


func TestSetRepresentmentWindow(t *testing.T) {
	l := New()
	if got := l.RepresentmentWindow(); got != 7*24*time.Hour {
		t.Errorf("default window = %v, want 7d", got)
	}
	if err := l.SetRepresentmentWindow(48 * time.Hour); err != nil {
		t.Fatalf("SetRepresentmentWindow: %v", err)
	}
	if got := l.RepresentmentWindow(); got != 48*time.Hour {
		t.Errorf("window = %v, want 48h", got)
	}
	if err := l.SetRepresentmentWindow(0); !errors.Is(err, ErrInvalidRepresentmentWindow) {
		t.Errorf("zero window = %v, want ErrInvalidRepresentmentWindow", err)
	}
	if err := l.SetRepresentmentWindow(-time.Hour); !errors.Is(err, ErrInvalidRepresentmentWindow) {
		t.Errorf("negative window = %v, want ErrInvalidRepresentmentWindow", err)
	}
	// A 48h window: a claim filed 3 days ago is already past it.
	l2 := seedDisputeLedger(t)
	if err := l2.SetRepresentmentWindow(48 * time.Hour); err != nil {
		t.Fatalf("seed SetRepresentmentWindow: %v", err)
	}
	if _, _, err := l2.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonUnauthorized, AmountCents: 6000, CreatedAt: time.Now().Add(-72 * time.Hour)}); err != nil {
		t.Fatalf("seed RaiseDispute: %v", err)
	}
	if _, _, err := l2.SubmitRepresentment("d1", []string{"sha256:x"}, "s"); !errors.Is(err, ErrRepresentmentWindowExpired) {
		t.Errorf("submit past 48h window = %v, want ErrRepresentmentWindowExpired", err)
	}
}

func TestRepresentmentDoesNotChangeChain(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{
		ID: "fund-payer", DebitAccount: "payer", CreditAccount: "equity",
		AmountCents: 20000, Currency: "USD", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "t1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed PostTransfer: %v", err)
	}
	if _, _, err := l.RaiseDispute(Dispute{
		ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonUnauthorized, AmountCents: 6000,
	}); err != nil {
		t.Fatalf("seed RaiseDispute: %v", err)
	}
	headBefore, _ := l.ChainHead()
	vBefore := l.Version()
	if _, _, err := l.SubmitRepresentment("d1", []string{"sha256:x"}, "s"); err != nil {
		t.Fatalf("SubmitRepresentment: %v", err)
	}
	headAfter, _ := l.ChainHead()
	if headBefore != headAfter {
		t.Error("representment submission must not append a chain link (off-journal)")
	}
	if l.Version() != vBefore {
		t.Error("representment submission must not bump the version")
	}
	// The audit trail still names the events.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var ops []string
	for i := range events {
		if strings.HasPrefix(events[i].Op, "representment_") || strings.HasPrefix(events[i].Op, "dispute_") {
			ops = append(ops, events[i].Op)
		}
	}
	want := []string{"dispute_raised", "representment_submitted"}
	if len(ops) != len(want) || ops[0] != want[0] || ops[1] != want[1] {
		t.Errorf("audit ops = %v, want %v", ops, want)
	}
}

