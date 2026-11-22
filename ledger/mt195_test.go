package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt195ExportLedger seeds a payout ledger with an audit log and the
// sender bank's BIC registered on the payee account, the fixture
// for the ExportMT195 tests.
func mt195ExportLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	l := seedPayoutLedger(t, WithAuditLog(al))
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "BANKUS33"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	return l
}

func mt195ExportOptions(t *testing.T) MT195ExportOptions {
	t.Helper()
	return MT195ExportOptions{
		ReceiverLT: "BANKDEFF",
		Queries:    "STAT",
		Narrative:  "Please confirm status of payment",
	}
}

func TestExportMT195_HappyPath(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ExportLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	before := l.Version()

	opts := mt195ExportOptions(t)
	res, err := l.ExportMT195(p.ID, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first export")
	}
	// :20: is the deterministic derivation: "MT195-" plus 10 hex.
	ref := mt195MessageReference(p.ID, opts.Queries, opts.Narrative)
	if res.Reference != ref {
		t.Errorf("reference = %q, want derived %q", res.Reference, ref)
	}
	if len(ref) != 16 || !strings.HasPrefix(ref, "MT195-") {
		t.Errorf("reference = %q, want 16 characters starting MT195-", ref)
	}
	if res.RelatedReference != p.ID || res.PayoutID != p.ID {
		t.Errorf("related/payout = %q/%q, want %q", res.RelatedReference, res.PayoutID, p.ID)
	}
	for _, want := range []string{
		"{1:F01BANKUS33XXX0000000000}",
		"{2:I195BANKDEFFXXXU3003}",
		"{3:{108:" + ref + "}}",
		":20:" + ref,
		":21:" + p.ID,
		":75:STAT",
		":79:Please confirm status of payment",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q:\n%s", want, res.Message)
		}
	}
	if strings.Contains(res.Message, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	// Round-trip: the export parses back as the same query.
	parsed, err := ParseMT195(res.Message)
	if err != nil {
		t.Fatalf("ParseMT195(rendered): %v", err)
	}
	if parsed.Reference != ref || parsed.RelatedReference != p.ID ||
		parsed.Queries != "STAT" || parsed.Narrative != "Please confirm status of payment" ||
		parsed.SenderLT != "BANKUS33XXX" || parsed.ReceiverLT != "BANKDEFFXXX" {
		t.Errorf("round-trip = %+v", parsed)
	}
	// Rendering is read-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// The stored export round-trips through the registry.
	stored, ok := l.GetMT195Export(ref)
	if !ok {
		t.Fatal("GetMT195Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.PayoutID != p.ID {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record.
	events := readAuditOps(t, al)
	if countOp(events, "mt195_query_exported") != 1 {
		t.Errorf("mt195_query_exported count = %d, want 1", countOp(events, "mt195_query_exported"))
	}
	ev := findOp(events, "mt195_query_exported")
	if ev == nil {
		t.Fatal("mt195_query_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator (ExportMT195)", ev.Actor)
	}
	if ev.TraceID != ref {
		t.Errorf("trace = %q, want %q", ev.TraceID, ref)
	}
	if ev.Details["msg_id"] != ref || ev.Details["related_reference"] != p.ID ||
		ev.Details["payout_id"] != p.ID {
		t.Errorf("details = %v, want msg/related/payout", ev.Details)
	}
}

func TestExportMT195_RepeatIsByteIdenticalAndDuplicate(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ExportLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-2", 250_000)

	opts := mt195ExportOptions(t)
	first, err := l.ExportMT195(p.ID, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195: %v", err)
	}
	second, err := l.ExportMT195(p.ID, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195 repeat: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on repeat export")
	}
	if second.Message != first.Message {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first.Message, second.Message)
	}
	// The replay emits no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "mt195_query_exported") != 1 {
		t.Errorf("mt195_query_exported count = %d, want 1 after replay", countOp(events, "mt195_query_exported"))
	}
}

func TestExportMT195_DifferentQuestionDifferentReference(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-3", 250_000)

	opts := mt195ExportOptions(t)
	first, err := l.ExportMT195(p.ID, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195: %v", err)
	}
	other := mt195ExportOptions(t)
	other.Narrative = "Please confirm value date"
	second, err := l.ExportMT195(p.ID, other, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195 other question: %v", err)
	}
	if second.Reference == first.Reference {
		t.Errorf("different question kept :20: %q, want a different reference", first.Reference)
	}
	if second.Duplicate {
		t.Error("duplicate = true on a different question")
	}
	// The derivation is stable: a fresh ledger derives the same
	// :20: for the same payout and question.
	l2 := mt103TestLedger(t)
	p2 := mt103SubmittedPayout(t, l2, "po-mt195-3", 250_000)
	again, err := l2.ExportMT195(p2.ID, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195 fresh ledger: %v", err)
	}
	if again.Reference != first.Reference {
		t.Errorf("fresh-ledger :20: = %q, want %q", again.Reference, first.Reference)
	}
}

func TestExportMT195_UnknownPayoutParks(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ExportLedger(t, al)

	opts := mt195ExportOptions(t)
	res, err := l.ExportMT195("no-such-payout", opts, "ledger-operator")
	if !errors.Is(err, ErrMT103PayoutNotFound) {
		t.Fatalf("err = %v, want ErrMT103PayoutNotFound", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id is empty, want the parked repair record")
	}
	ref := mt195MessageReference("no-such-payout", opts.Queries, opts.Narrative)
	if res.Reference != ref {
		t.Errorf("reference = %q, want derived %q", res.Reference, ref)
	}
	// A parked export records nothing: no message to replay.
	if _, ok := l.GetMT195Export(ref); ok {
		t.Error("GetMT195Export = true for a parked export, want false")
	}
	// ... and no export marker.
	events := readAuditOps(t, al)
	if countOp(events, "mt195_query_exported") != 0 {
		t.Errorf("mt195_query_exported count = %d, want 0 for a parked export", countOp(events, "mt195_query_exported"))
	}
}

func TestExportMT195_NoQuestion(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-4", 250_000)

	opts := mt195ExportOptions(t)
	opts.Queries = ""
	opts.Narrative = ""
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195Field) {
		t.Errorf("err = %v, want ErrMT195Field (a query must ask something)", err)
	}
	// Whitespace-only question text is no question either.
	opts.Narrative = "   \n  "
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195Field) {
		t.Errorf("err = %v, want ErrMT195Field for a whitespace-only question", err)
	}
}

func TestExportMT195_NoSenderBIC(t *testing.T) {
	l := seedPayoutLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-5", 250_000)

	// No SetAccountIBAN: the sender BIC is not configured.
	if _, err := l.ExportMT195(p.ID, mt195ExportOptions(t), "ledger-operator"); !errors.Is(err, ErrMT195Export) {
		t.Errorf("err = %v, want ErrMT195Export (no sender BIC)", err)
	}
}

func TestExportMT195_BadReceiverLT(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-6", 250_000)

	opts := mt195ExportOptions(t)
	opts.ReceiverLT = ""
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195Export) {
		t.Errorf("empty receiver: err = %v, want ErrMT195Export", err)
	}
	opts.ReceiverLT = "NOT-A-BIC"
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195Export) {
		t.Errorf("invalid receiver: err = %v, want ErrMT195Export", err)
	}
}

func TestExportMT195_QuestionLineViolations(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-7", 250_000)

	// A :75: line over 35 characters is refused, never truncated.
	opts := mt195ExportOptions(t)
	opts.Queries = strings.Repeat("x", 36)
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195Field) {
		t.Errorf("overlong :75: line: err = %v, want ErrMT195Field", err)
	}
	// Seven :75: lines exceed the 6*35x shape.
	opts.Queries = "L1\nL2\nL3\nL4\nL5\nL6\nL7"
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195Field) {
		t.Errorf("7 :75: lines: err = %v, want ErrMT195Field", err)
	}
	// A brace in the :79: narrative would break FIN framing.
	opts = mt195ExportOptions(t)
	opts.Narrative = "Please confirm {status}"
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195Field) {
		t.Errorf("brace in :79:: err = %v, want ErrMT195Field", err)
	}
}

func TestExportMT195_OneFieldOnly(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-8", 250_000)

	// :75: only: :79: is omitted, and the message still parses.
	queriesOnly := mt195ExportOptions(t)
	queriesOnly.Narrative = ""
	res, err := l.ExportMT195(p.ID, queriesOnly, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195 :75:-only: %v", err)
	}
	if strings.Contains(res.Message, ":79:") {
		t.Error(":79: must be omitted when the narrative is empty")
	}
	parsed, err := ParseMT195(res.Message)
	if err != nil {
		t.Fatalf("ParseMT195(:75:-only): %v", err)
	}
	if parsed.Queries != "STAT" || parsed.Narrative != "" {
		t.Errorf("round-trip = %+v", parsed)
	}

	// :79: only: :75: is omitted, and the message still parses.
	narrativeOnly := mt195ExportOptions(t)
	narrativeOnly.Queries = ""
	res, err = l.ExportMT195(p.ID, narrativeOnly, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195 :79:-only: %v", err)
	}
	if strings.Contains(res.Message, ":75:") {
		t.Error(":75: must be omitted when the coded question is empty")
	}
	parsed, err = ParseMT195(res.Message)
	if err != nil {
		t.Fatalf("ParseMT195(:79:-only): %v", err)
	}
	if parsed.Queries != "" || parsed.Narrative != "Please confirm status of payment" {
		t.Errorf("round-trip = %+v", parsed)
	}
}

func TestExportMT195_EmptyPayoutID(t *testing.T) {
	l := mt103TestLedger(t)
	if _, err := l.ExportMT195("", mt195ExportOptions(t), "ledger-operator"); !errors.Is(err, ErrMT195Reference) {
		t.Errorf("err = %v, want ErrMT195Reference for an empty :21:", err)
	}
}

func TestExportMT195_Conflict(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-9", 250_000)

	opts := mt195ExportOptions(t)
	ref := mt195MessageReference(p.ID, opts.Queries, opts.Narrative)
	// A :20: already exported under different terms: the reference
	// names one export, so a second, different question under it
	// is refused rather than silently overwritten. The derivation
	// makes this unreachable through the public API (the same
	// terms hash to the same :20:), so the conflicting record is
	// seeded directly to exercise the gate itself.
	l.mt195Exports = map[string]mt195ExportRecord{
		ref: {
			PayoutID: p.ID,
			Options: MT195ExportOptions{
				ReceiverLT: "BANKDEFF",
				Queries:    "STAT",
				Narrative:  "A different question",
			},
			Result: MT195ExportResult{Reference: ref, RelatedReference: p.ID, PayoutID: p.ID, Message: "seeded"},
		},
	}
	if _, err := l.ExportMT195(p.ID, opts, "ledger-operator"); !errors.Is(err, ErrMT195MsgIDConflict) {
		t.Errorf("err = %v, want ErrMT195MsgIDConflict", err)
	}
}
