package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt192Doc renders one inbound SWIFT FIN MT192 message: ref as
// :20:, payoutID as :21:, valueDate11S as the :11S: body
// ("103" + YYMMDD), and reason as the :79: narrative (a reason
// containing "\n" spans lines, the export's own rendering). An
// empty reason omits :79: entirely.
func mt192Doc(ref, payoutID, valueDate11S, reason string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I192BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	sb.WriteString(":11S:" + valueDate11S + "\n")
	if reason != "" {
		sb.WriteString(":79:" + reason + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// mt192DocForPayout renders the MT192 cancelling submitted payout
// p: the :11S: is the payout's MT103 value date by the export's
// default rule (submit time), the :20: is ref, the reason rides
// verbatim.
func mt192DocForPayout(p PayoutInstruction, ref, reason string) string {
	return mt192Doc(ref, p.ID, "103"+mt103ValueDate(p.SubmittedAt), reason)
}

// applyMT192Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT192Doc(t *testing.T, l *Ledger, doc string) (MT192ApplyResult, error) {
	t.Helper()
	m, err := ParseMT192(doc)
	if err != nil {
		t.Fatalf("ParseMT192: %v", err)
	}
	return l.ApplyMT192(m, "correspondent-bank")
}

// mt192ApplyLedger seeds a payout ledger with an audit log, the
// fixture for the MT192 apply tests.
func mt192ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

func TestParseMT192_RoundTrip(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)

	msg, err := l.ExportMT192(MT192ExportOptions{
		PayoutID:   p.ID,
		ReceiverLT: "BANKDEFFXXXX",
		Reason:     "Duplicate payment, please cancel",
	})
	if err != nil {
		t.Fatalf("ExportMT192: %v", err)
	}
	m, err := ParseMT192(msg)
	if err != nil {
		t.Fatalf("ParseMT192(export): %v", err)
	}
	if m.SenderLT != "BANKUS33XXX" || m.ReceiverLT != "BANKDEFFXXXX" {
		t.Errorf("LTs = %q/%q, want BANKUS33XXX/BANKDEFFXXXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != mt192MessageReference(p.ID) {
		t.Errorf(":20: = %q, want %q", m.Reference, mt192MessageReference(p.ID))
	}
	if m.RelatedReference != p.ID {
		t.Errorf(":21: = %q, want %q", m.RelatedReference, p.ID)
	}
	if got := mt103ValueDate(m.ValueDate); got != mt103ValueDate(p.SubmittedAt) {
		t.Errorf(":11S: = %q, want %q", got, mt103ValueDate(p.SubmittedAt))
	}
	if m.Reason != "Duplicate payment, please cancel" {
		t.Errorf(":79: = %q, want the reason verbatim", m.Reason)
	}
	// A multi-line :79: round-trips line for line.
	msg, err = l.ExportMT192(MT192ExportOptions{
		PayoutID:   p.ID,
		ReceiverLT: "BANKDEFFXXXX",
		Reason:     "Duplicate payment\nWrong beneficiary",
	})
	if err != nil {
		t.Fatalf("ExportMT192 multi-line: %v", err)
	}
	m, err = ParseMT192(msg)
	if err != nil {
		t.Fatalf("ParseMT192(multi-line export): %v", err)
	}
	if m.Reason != "Duplicate payment\nWrong beneficiary" {
		t.Errorf(":79: = %q, want two lines verbatim", m.Reason)
	}
}

func TestParseMT192_RejectsBadBlockStructure(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	good := mt192DocForPayout(p, "MT192APPLY00001", "Duplicate payment")

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT202", strings.Replace(good, "{2:I192", "{2:I202", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:MT192APPLY00001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":20:MT192APPLY00001\n:21:po-mt192-1\n",
			":21:po-mt192-1\n:20:MT192APPLY00001\n", 1)},
		{"missing :11S:", strings.Replace(good, ":11S:103"+mt103ValueDate(p.SubmittedAt)+"\n", "", 1)},
		{"empty :20:", strings.Replace(good, ":20:MT192APPLY00001\n", ":20:\n", 1)},
		{":11S: wrong message type", mt192Doc("MT192APPLY00001", p.ID, "104"+mt103ValueDate(p.SubmittedAt), "Duplicate payment")},
		{":11S: impossible date", mt192Doc("MT192APPLY00001", p.ID, "103991331", "Duplicate payment")},
		{"too few lines", "{1:F01BANKUS33XXX0000000000}"},
		{"empty document", "  "},
	}
	for _, tc := range cases {
		if _, err := ParseMT192(tc.doc); err == nil {
			t.Errorf("%s: parsed without error, want refusal", tc.name)
		}
	}
	// The :11S: cases carry the value-date identity; the rest
	// are framing or field errors.
	if _, err := ParseMT192(mt192Doc("MT192APPLY00001", p.ID, "104"+mt103ValueDate(p.SubmittedAt), "Duplicate payment")); !errors.Is(err, ErrMT192ValueDate) {
		t.Errorf("wrong message type: err = %v, want ErrMT192ValueDate", err)
	}
	if _, err := ParseMT192(mt192Doc("MT192APPLY00001", p.ID, "103991331", "Duplicate payment")); !errors.Is(err, ErrMT192ValueDate) {
		t.Errorf("impossible date: err = %v, want ErrMT192ValueDate", err)
	}
}

func TestParseMT192_RejectsUnknownField(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	good := mt192DocForPayout(p, "MT192APPLY00001", "Duplicate payment")

	// An optional :72: sender-to-receiver field is out of the
	// core set: refused, not parsed with the field quietly
	// discarded.
	with72 := strings.Replace(good, "-}", ":72:/BNK/Do not cancel\n-}", 1)
	if _, err := ParseMT192(with72); !errors.Is(err, ErrMT192Field) {
		t.Errorf(":72: field: err = %v, want ErrMT192Field", err)
	}
	// A `:` line inside the :79: narrative run is an unknown
	// field, not a continuation.
	withColon := strings.Replace(good, ":79:Duplicate payment\n", ":79:Duplicate payment\n:99:extra\n", 1)
	if _, err := ParseMT192(withColon); !errors.Is(err, ErrMT192Field) {
		t.Errorf(":99: in narrative: err = %v, want ErrMT192Field", err)
	}
}

func TestParseMT192_AcceptsCRLF(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	doc := mt192DocForPayout(p, "MT192APPLY00001", "Duplicate payment")

	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT192(crlf); err != nil {
		t.Errorf("CRLF: %v", err)
	}
	// A trailing newline after the trailer is tolerated.
	if _, err := ParseMT192(doc + "\n"); err != nil {
		t.Errorf("trailing newline: %v", err)
	}
}

func TestApplyMT192_CancelsSubmittedPayout(t *testing.T) {
	al := mustAuditLog(t)
	l := mt192ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	before := l.Version()
	reason := "Duplicate payment, please cancel"

	res, err := applyMT192Doc(t, l, mt192DocForPayout(p, "MT192APPLY00001", reason))
	if err != nil {
		t.Fatalf("ApplyMT192: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "MT192APPLY00001" {
		t.Errorf("reference = %q, want MT192APPLY00001", res.Reference)
	}
	c := res.Cancellation
	if c.Status != CancellationRequested {
		t.Errorf("cancellation status = %q, want requested", c.Status)
	}
	if c.PayoutID != p.ID || c.UETR != p.UETR {
		t.Errorf("cancellation = %+v, want payout %q / uetr %q", c, p.ID, p.UETR)
	}
	if c.ReasonCode != reason {
		t.Errorf("reason = %q, want verbatim", c.ReasonCode)
	}
	// The payout itself stays submitted until the bank confirms:
	// requesting a cancellation is not the cancellation.
	if got, ok := l.GetPayout(p.ID); !ok || got.Status != PayoutSubmitted {
		t.Errorf("stored payout = %+v, want still submitted", got)
	}
	// Cancelling is structural: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (structural, no bump)", l.Version(), before)
	}
	// Audit: the entry's event plus the inbound marker, each
	// exactly once, with the exact details — the :20: on the
	// durable record.
	events := readAuditOps(t, al)
	if countOp(events, "cancellation_requested") != 1 {
		t.Errorf("cancellation_requested count = %d, want 1", countOp(events, "cancellation_requested"))
	}
	if countOp(events, "mt192_request_applied") != 1 {
		t.Errorf("mt192_request_applied count = %d, want 1", countOp(events, "mt192_request_applied"))
	}
	ev := findOp(events, "mt192_request_applied")
	if ev == nil {
		t.Fatal("mt192_request_applied event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT192)", ev.Actor)
	}
	if ev.TraceID != "MT192APPLY00001" {
		t.Errorf("trace = %q, want MT192APPLY00001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "MT192APPLY00001" || ev.Details["payout_id"] != p.ID ||
		ev.Details["uetr"] != p.UETR || ev.Details["reason_code"] != reason ||
		ev.Details["value_date"] != mt103ValueDate(p.SubmittedAt) {
		t.Errorf("details = %v, want msg/payout/uetr/reason/value-date", ev.Details)
	}
}

func TestApplyMT192_RefusesUnknownPayout(t *testing.T) {
	al := mustAuditLog(t)
	l := mt192ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	doc := mt192Doc("MT192APPLY00001", "no-such", "103"+mt103ValueDate(p.SubmittedAt), "Duplicate payment")
	_, err := applyMT192Doc(t, l, doc)
	if !errors.Is(err, ErrMT103PayoutNotFound) {
		t.Errorf("err = %v, want ErrMT103PayoutNotFound", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "cancellation_requested") != 0 || countOp(events, "mt192_request_applied") != 0 {
		t.Errorf("cancellation events = %d/%d, want 0/0",
			countOp(events, "cancellation_requested"), countOp(events, "mt192_request_applied"))
	}
}

func TestApplyMT192_RefusesApprovedPayoutNotIssued(t *testing.T) {
	al := mustAuditLog(t)
	l := mt192ApplyLedger(t, al)
	draftPayout(t, l, "po-mt192-2", 250_000)
	p, err := l.ApprovePayout("po-mt192-2", "bob")
	if err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	// Approved, never submitted: the instruction never left the
	// ledger — CancelPayout, not MT192, is the path.
	doc := mt192Doc("MT192APPLY00002", p.ID, "103"+mt103ValueDate(p.ApprovedAt), "Duplicate payment")
	_, err = applyMT192Doc(t, l, doc)
	if !errors.Is(err, ErrCancellationNotIssued) {
		t.Errorf("err = %v, want ErrCancellationNotIssued", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "cancellation_requested") != 0 || countOp(events, "mt192_request_applied") != 0 {
		t.Errorf("cancellation events = %d/%d, want 0/0",
			countOp(events, "cancellation_requested"), countOp(events, "mt192_request_applied"))
	}
}

func TestApplyMT192_RefusesSettledPayout(t *testing.T) {
	al := mustAuditLog(t)
	l := mt192ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt192-3", 250_000)
	if _, err := l.SettlePayout(p.ID, statementLineForPayout(p, p.SubmittedAt), StatementMatchOptions{}); err != nil {
		t.Fatalf("SettlePayout: %v", err)
	}
	// Settled: the money already moved — cancel via pacs.007,
	// never through this path.
	_, err := applyMT192Doc(t, l, mt192DocForPayout(p, "MT192APPLY00003", "Duplicate payment"))
	if !errors.Is(err, ErrCancellationSettled) {
		t.Errorf("err = %v, want ErrCancellationSettled", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt192_request_applied") != 0 {
		t.Errorf("mt192_request_applied count = %d, want 0", countOp(events, "mt192_request_applied"))
	}
}

func TestApplyMT192_RefusesValueDateMismatch(t *testing.T) {
	al := mustAuditLog(t)
	l := mt192ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	// The :11S: names tomorrow; the payout's MT103 value date is
	// the submit day. The request points at another instruction.
	tomorrow := mt103ValueDate(p.SubmittedAt.AddDate(0, 0, 1))
	doc := mt192Doc("MT192APPLY00001", p.ID, "103"+tomorrow, "Duplicate payment")
	_, err := applyMT192Doc(t, l, doc)
	if !errors.Is(err, ErrMT192ValueDateMismatch) {
		t.Errorf("err = %v, want ErrMT192ValueDateMismatch", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "cancellation_requested") != 0 || countOp(events, "mt192_request_applied") != 0 {
		t.Errorf("cancellation events = %d/%d, want 0/0",
			countOp(events, "cancellation_requested"), countOp(events, "mt192_request_applied"))
	}
	if got, ok := l.GetPayout(p.ID); !ok || got.Status != PayoutSubmitted {
		t.Errorf("stored payout = %+v, want still submitted", got)
	}
}

func TestApplyMT192_RefusesMissingReason(t *testing.T) {
	al := mustAuditLog(t)
	l := mt192ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	// No :79: block at all: a cancellation without a reason
	// cannot be executed (the camt.056 reason-required gate).
	_, err := applyMT192Doc(t, l, mt192Doc("MT192APPLY00001", p.ID, "103"+mt103ValueDate(p.SubmittedAt), ""))
	if !errors.Is(err, ErrCancellationReasonRequired) {
		t.Errorf("err = %v, want ErrCancellationReasonRequired", err)
	}
	// A hand-built request with no reason is refused before
	// anything moves: the parse gate and the apply gate share
	// one check.
	m, err := ParseMT192(mt192DocForPayout(p, "MT192APPLY00001", "Duplicate payment"))
	if err != nil {
		t.Fatalf("ParseMT192: %v", err)
	}
	m.Reason = "   "
	if _, err := l.ApplyMT192(m, "correspondent-bank"); !errors.Is(err, ErrCancellationReasonRequired) {
		t.Errorf("blank reason: err = %v, want ErrCancellationReasonRequired", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "cancellation_requested") != 0 {
		t.Errorf("cancellation_requested count = %d, want 0", countOp(events, "cancellation_requested"))
	}
}

func TestApplyMT192_ReplaySameMsgIdNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt192ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)
	doc := mt192DocForPayout(p, "MT192APPLY00001", "Duplicate payment")
	if _, err := applyMT192Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyMT192: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// current cancellation record, with no second audit event.
	res, err := applyMT192Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT192 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Cancellation.Status != CancellationRequested {
		t.Errorf("cancellation status = %q, want requested", res.Cancellation.Status)
	}
	events := readAuditOps(t, al)
	if countOp(events, "cancellation_requested") != 1 || countOp(events, "mt192_request_applied") != 1 {
		t.Errorf("requested/applied events = %d/%d, want 1/1 after replay",
			countOp(events, "cancellation_requested"), countOp(events, "mt192_request_applied"))
	}
	// Same :20:, different reason: a conflict, never a silent
	// overwrite.
	conflict := mt192DocForPayout(p, "MT192APPLY00001", "A different reason")
	if _, err := applyMT192Doc(t, l, conflict); !errors.Is(err, ErrMT192MsgIDConflict) {
		t.Errorf("conflict reason: err = %v, want ErrMT192MsgIDConflict", err)
	}
	// Same :20:, different value date: a conflict too.
	tomorrow := mt103ValueDate(p.SubmittedAt.AddDate(0, 0, 1))
	conflict = mt192Doc("MT192APPLY00001", p.ID, "103"+tomorrow, "Duplicate payment")
	if _, err := applyMT192Doc(t, l, conflict); !errors.Is(err, ErrMT192MsgIDConflict) {
		t.Errorf("conflict value date: err = %v, want ErrMT192MsgIDConflict", err)
	}
	events = readAuditOps(t, al)
	if countOp(events, "cancellation_requested") != 1 || countOp(events, "mt192_request_applied") != 1 {
		t.Errorf("requested/applied events = %d/%d, want 1/1 after conflicts",
			countOp(events, "cancellation_requested"), countOp(events, "mt192_request_applied"))
	}
}
