package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The export renders a payout's unable-to-apply report and the
// render round-trips through the inbound parser: the message the
// ledger sends is one the ledger itself can read back.
func TestExportCAMT026_PayoutRoundTrip(t *testing.T) {
	l, al := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-exp-1")

	res, err := l.ExportCAMT026(p.UETR, CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS01",
		ReasonInfo:    "missing creditor account",
		CaseID:        "case-9",
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026: %v", err)
	}
	if res.Kind != CAMT026AssociatedPayout || res.TargetID != p.ID {
		t.Errorf("association = %q/%q, want payout/po-026-exp-1", res.Kind, res.TargetID)
	}
	if !strings.HasPrefix(res.MsgID, "CAMT026-") || len(res.MsgID) != 18 {
		t.Errorf("msg_id = %q, want CAMT026- + 10 hex", res.MsgID)
	}
	if res.UETR != p.UETR || res.EndToEndID != p.ID {
		t.Errorf("references = %q/%q, want %q/%q", res.UETR, res.EndToEndID, p.UETR, p.ID)
	}
	if res.Duplicate {
		t.Error("first export reports Duplicate")
	}
	if !strings.Contains(res.Message, "camt.026.001.07") || !strings.Contains(res.Message, "<UblToApply>") {
		t.Errorf("message is not a camt.026 document:\n%s", res.Message)
	}

	rep, err := ParseCAMT026([]byte(res.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026(own export): %v", err)
	}
	if rep.MsgID != res.MsgID || rep.UETR != p.UETR || rep.EndToEndID != p.ID {
		t.Errorf("round-trip references = %+v, want msg %q uetr %q e2e %q", rep, res.MsgID, p.UETR, p.ID)
	}
	if rep.UnderlyingKind != CAMT026UnderlyingInitiation {
		t.Errorf("round-trip kind = %q, want initiation", rep.UnderlyingKind)
	}
	if rep.Justification != CAMT026JustificationMissingOrIncorrect || rep.ReasonCode != "MS01" || rep.ReasonInfo != "missing creditor account" {
		t.Errorf("round-trip reason = %q/%q/%q, want missing-or-incorrect/MS01/missing creditor account", rep.Justification, rep.ReasonCode, rep.ReasonInfo)
	}
	if rep.CaseID != "case-9" {
		t.Errorf("round-trip case = %q, want case-9", rep.CaseID)
	}
	if !rep.HasAmount || rep.AmountCents != 250_000 || rep.Currency != "USD" {
		t.Errorf("round-trip amount = %v/%d/%q, want true/250000/USD", rep.HasAmount, rep.AmountCents, rep.Currency)
	}
	// No IBAN is registered in the fixture: the assignor defaults
	// to the payee account, the assignee to the payout destination.
	if rep.Assignor != "saver" || rep.Assignee != "FPS:6222000012345678" {
		t.Errorf("round-trip assignor/assignee = %q/%q, want saver/FPS:6222000012345678", rep.Assignor, rep.Assignee)
	}

	// The payout is untouched: still submitted, same amount.
	if got, _ := l.GetPayout(p.ID); got.Status != PayoutSubmitted {
		t.Errorf("payout status = %q, want submitted", got.Status)
	}
	// The record round-trips through the getter.
	if got, ok := l.GetCAMT026Export(res.MsgID); !ok || got.TargetID != p.ID || got.Message != res.Message {
		t.Errorf("GetCAMT026Export = %+v, want the recorded payout export", got)
	}

	events := readAuditOps(t, al)
	if countOp(events, "camt026_unable_to_apply_exported") != 1 {
		t.Errorf("camt026_unable_to_apply_exported count = %d, want 1", countOp(events, "camt026_unable_to_apply_exported"))
	}
	ev := findOp(events, "camt026_unable_to_apply_exported")
	if ev == nil {
		t.Fatal("camt026_unable_to_apply_exported event missing")
	}
	if ev.TraceID != res.MsgID || ev.Actor != "bank-ops" {
		t.Errorf("trace/actor = %q/%q, want %q/bank-ops", ev.TraceID, ev.Actor, res.MsgID)
	}
	if ev.Details["kind"] != "payout" || ev.Details["target_id"] != "po-026-exp-1" {
		t.Errorf("details = %v, want kind payout target po-026-exp-1", ev.Details)
	}
	if ev.Details["uetr"] != p.UETR || ev.Details["end_to_end_id"] != p.ID {
		t.Errorf("details = %v, want uetr %q e2e %q", ev.Details, p.UETR, p.ID)
	}
	if ev.Details["justification"] != "missing-or-incorrect" || ev.Details["reason_code"] != "MS01" || ev.Details["case_id"] != "case-9" {
		t.Errorf("details = %v, want justification missing-or-incorrect reason MS01 case case-9", ev.Details)
	}
}

// The MsgId is a deterministic derivation of the instruction and
// the reason: two ledgers reporting the same payout for the same
// reason derive the same MsgId, even though the payouts' UETRs
// differ.
func TestExportCAMT026_MsgIDDeterministic(t *testing.T) {
	opts := CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS01",
		ReasonInfo:    "missing creditor account",
	}
	l1, _ := camt026Fixture(t)
	p1 := submittedPayout(t, l1, "po-026-exp-2")
	res1, err := l1.ExportCAMT026(p1.UETR, opts, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (ledger 1): %v", err)
	}
	l2, _ := camt026Fixture(t)
	p2 := submittedPayout(t, l2, "po-026-exp-2")
	res2, err := l2.ExportCAMT026(p2.UETR, opts, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (ledger 2): %v", err)
	}
	if res1.MsgID != res2.MsgID {
		t.Errorf("msg_id = %q vs %q, want the same deterministic MsgId", res1.MsgID, res2.MsgID)
	}
	if want := camt026ExportMsgID(CAMT026AssociatedPayout, "po-026-exp-2", opts); res1.MsgID != want {
		t.Errorf("msg_id = %q, want derived %q", res1.MsgID, want)
	}
}

// A repeat export is byte-identical and a no-op: the stored
// message returns with Duplicate set and no second audit event.
func TestExportCAMT026_RepeatIsByteIdenticalAndDuplicate(t *testing.T) {
	l, al := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-exp-3")
	opts := CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS01",
		ReasonInfo:    "missing creditor account",
	}
	first, err := l.ExportCAMT026(p.UETR, opts, "bank-ops")
	if err != nil {
		t.Fatalf("first ExportCAMT026: %v", err)
	}
	second, err := l.ExportCAMT026(p.UETR, opts, "bank-ops")
	if err != nil {
		t.Fatalf("second ExportCAMT026: %v", err)
	}
	if !second.Duplicate {
		t.Error("repeat export reports Duplicate false, want true")
	}
	if second.MsgID != first.MsgID || second.Message != first.Message {
		t.Error("repeat export rendered different bytes, want byte-identical")
	}
	if countOp(readAuditOps(t, al), "camt026_unable_to_apply_exported") != 1 {
		t.Error("repeat export emitted a second audit event, want exactly 1")
	}
}

// A transfer renders the IntrBk arm and an RfP the Initn arm, each
// round-tripping through the inbound parser.
func TestExportCAMT026_TransferAndRfP(t *testing.T) {
	l, _ := camt026Fixture(t)
	if _, err := l.PostTransfer(Transfer{ID: "tx-026-exp-1", From: "saver", To: "merchant", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	res, err := l.ExportCAMT026("tx-026-exp-1", CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "AM09",
		ReasonInfo:    "wrong amount",
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (transfer): %v", err)
	}
	if res.Kind != CAMT026AssociatedTransfer || res.TargetID != "tx-026-exp-1" {
		t.Errorf("association = %q/%q, want transfer/tx-026-exp-1", res.Kind, res.TargetID)
	}
	rep, err := ParseCAMT026([]byte(res.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026 (transfer export): %v", err)
	}
	if rep.UnderlyingKind != CAMT026UnderlyingInterbank {
		t.Errorf("round-trip kind = %q, want interbank", rep.UnderlyingKind)
	}
	if rep.EndToEndID != "tx-026-exp-1" || rep.UETR != "" {
		t.Errorf("round-trip references = %q/%q, want tx-026-exp-1/empty (no gpi track names this transfer)", rep.EndToEndID, rep.UETR)
	}
	if !rep.HasAmount || rep.AmountCents != 1000 || rep.Currency != "USD" {
		t.Errorf("round-trip amount = %v/%d/%q, want true/1000/USD", rep.HasAmount, rep.AmountCents, rep.Currency)
	}
	if rep.ReasonCode != "AM09" || rep.ReasonInfo != "wrong amount" {
		t.Errorf("round-trip reason = %q/%q, want AM09/wrong amount", rep.ReasonCode, rep.ReasonInfo)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000 (no money moves)", got)
	}

	ctx := context.Background()
	if _, dup, err := l.CreateRfP(ctx, RfP{ID: "rfp-026-exp-1", Creditor: "merchant", Debtor: "saver",
		AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)}); err != nil || dup {
		t.Fatalf("CreateRfP: %v dup=%v", err, dup)
	}
	res, err = l.ExportCAMT026("rfp-026-exp-1", CAMT026ExportOptions{
		Justification: CAMT026JustificationAnyInformation,
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (RfP): %v", err)
	}
	if res.Kind != CAMT026AssociatedRfP || res.TargetID != "rfp-026-exp-1" {
		t.Errorf("association = %q/%q, want rfp/rfp-026-exp-1", res.Kind, res.TargetID)
	}
	rep, err = ParseCAMT026([]byte(res.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026 (RfP export): %v", err)
	}
	if rep.UnderlyingKind != CAMT026UnderlyingInitiation {
		t.Errorf("round-trip kind = %q, want initiation", rep.UnderlyingKind)
	}
	if rep.EndToEndID != "rfp-026-exp-1" || rep.UETR != "" {
		t.Errorf("round-trip references = %q/%q, want rfp-026-exp-1/empty (an RfP carries no UETR)", rep.EndToEndID, rep.UETR)
	}
	if rep.Justification != CAMT026JustificationAnyInformation || rep.ReasonCode != "" {
		t.Errorf("round-trip reason = %q/%q, want any-information/empty", rep.Justification, rep.ReasonCode)
	}
}

// All three justification arms render and round-trip.
func TestExportCAMT026_JustificationArms(t *testing.T) {
	l, _ := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-exp-4")

	anyRes, err := l.ExportCAMT026(p.UETR, CAMT026ExportOptions{
		Justification: CAMT026JustificationAnyInformation,
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (any-information): %v", err)
	}
	if !strings.Contains(anyRes.Message, "<AnyInf>true</AnyInf>") {
		t.Errorf("any-information export missing <AnyInf>:\n%s", anyRes.Message)
	}
	rep, err := ParseCAMT026([]byte(anyRes.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026 (any-information): %v", err)
	}
	if rep.Justification != CAMT026JustificationAnyInformation || rep.ReasonCode != "" || rep.ReasonInfo != "" {
		t.Errorf("round-trip = %q/%q/%q, want any-information/empty/empty", rep.Justification, rep.ReasonCode, rep.ReasonInfo)
	}

	dupRes, err := l.ExportCAMT026(p.UETR, CAMT026ExportOptions{
		Justification: CAMT026JustificationPossibleDuplicate,
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (possible-duplicate): %v", err)
	}
	if !strings.Contains(dupRes.Message, "<PssblDplctInstr>true</PssblDplctInstr>") {
		t.Errorf("possible-duplicate export missing <PssblDplctInstr>:\n%s", dupRes.Message)
	}
	rep, err = ParseCAMT026([]byte(dupRes.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026 (possible-duplicate): %v", err)
	}
	if rep.Justification != CAMT026JustificationPossibleDuplicate {
		t.Errorf("round-trip justification = %q, want possible-duplicate", rep.Justification)
	}
	if dupRes.MsgID == anyRes.MsgID {
		t.Error("different justifications derived the same MsgId, want distinct exports")
	}
}

// Export validation fail-closes: no reference, no justification,
// a justification value outside the three arms, a
// missing-or-incorrect justification with no reason code, and a
// reason code or info on an arm that carries none.
func TestExportCAMT026_Validation(t *testing.T) {
	l, _ := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-exp-5")
	missing := CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS01",
	}
	cases := []struct {
		name string
		ref  string
		opts CAMT026ExportOptions
	}{
		{"empty reference", "", missing},
		{"blank reference", "   ", missing},
		{"missing justification", p.UETR, CAMT026ExportOptions{ReasonCode: "MS01"}},
		{"invalid justification", p.UETR, CAMT026ExportOptions{Justification: "unknown-arm", ReasonCode: "MS01"}},
		{"missing-or-incorrect without code", p.UETR, CAMT026ExportOptions{Justification: CAMT026JustificationMissingOrIncorrect}},
		{"any-information with code", p.UETR, CAMT026ExportOptions{Justification: CAMT026JustificationAnyInformation, ReasonCode: "MS01"}},
		{"any-information with info", p.UETR, CAMT026ExportOptions{Justification: CAMT026JustificationAnyInformation, ReasonInfo: "extra"}},
		{"possible-duplicate with code", p.UETR, CAMT026ExportOptions{Justification: CAMT026JustificationPossibleDuplicate, ReasonCode: "MS01"}},
	}
	for _, tc := range cases {
		if _, err := l.ExportCAMT026(tc.ref, tc.opts, "bank-ops"); !errors.Is(err, ErrCAMT026Export) {
			t.Errorf("%s: err = %v, want ErrCAMT026Export", tc.name, err)
		}
	}
	// None of the refused exports recorded anything.
	if _, ok := l.GetCAMT026Export(camt026ExportMsgID(CAMT026AssociatedPayout, p.ID, missing)); ok {
		t.Error("a refused export left a registry record")
	}
}

// An unknown reference is refused fail-closed, and the refusal
// writes no state.
func TestExportCAMT026_UnknownReference(t *testing.T) {
	l, al := camt026Fixture(t)
	if _, err := l.ExportCAMT026("no-such-uetr", CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS01",
	}, "bank-ops"); !errors.Is(err, ErrCAMT026UnknownReference) {
		t.Errorf("err = %v, want ErrCAMT026UnknownReference", err)
	}
	if countOp(readAuditOps(t, al), "camt026_unable_to_apply_exported") != 0 {
		t.Error("a refused export emitted an audit event")
	}
}

// A MsgId already exported under different terms is refused
// rather than silently overwritten. The derivation makes this
// unreachable through the public API (the same terms hash to the
// same MsgId), so the conflicting record is seeded directly to
// exercise the gate itself — the MT195 conflict-test precedent.
func TestExportCAMT026_Conflict(t *testing.T) {
	l, _ := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-exp-6")

	opts := CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS01",
		ReasonInfo:    "missing creditor account",
	}
	msgID := camt026ExportMsgID(CAMT026AssociatedPayout, p.ID, opts)
	l.camt026Exports = map[string]camt026ExportRecord{
		msgID: {
			Kind:     CAMT026AssociatedPayout,
			TargetID: p.ID,
			Options: CAMT026ExportOptions{
				Justification: CAMT026JustificationMissingOrIncorrect,
				ReasonCode:    "MS02",
				ReasonInfo:    "a different reason",
			},
			Result: CAMT026ExportResult{MsgID: msgID, TargetID: p.ID, Message: "seeded"},
		},
	}
	if _, err := l.ExportCAMT026(p.UETR, opts, "bank-ops"); !errors.Is(err, ErrCAMT026MsgIDConflict) {
		t.Errorf("err = %v, want ErrCAMT026MsgIDConflict", err)
	}

	// A different reason about the same instruction is a different
	// export under a different MsgId, not a conflict.
	other, err := l.ExportCAMT026(p.UETR, CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS02",
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (different reason): %v", err)
	}
	if other.MsgID == msgID {
		t.Error("a different reason derived the same MsgId")
	}
	if other.Duplicate {
		t.Error("a different reason reports Duplicate")
	}
}

// The assignor defaults to the registered IBAN BIC when one
// exists, and the operator may name both envelope parties — but
// envelope names are not terms: a replay under different names
// returns the first rendering.
func TestExportCAMT026_EnvelopeNames(t *testing.T) {
	l, _ := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-exp-7")
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	opts := CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS01",
	}
	first, err := l.ExportCAMT026(p.UETR, opts, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026: %v", err)
	}
	rep, err := ParseCAMT026([]byte(first.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	if rep.Assignor != "DEUTDEFF" {
		t.Errorf("assignor = %q, want the registered BIC DEUTDEFF", rep.Assignor)
	}
	if rep.Assignee != "FPS:6222000012345678" {
		t.Errorf("assignee = %q, want the payout destination", rep.Assignee)
	}

	// Operator-named envelope parties render, and a replay under
	// different names is still the first rendering.
	named, err := l.ExportCAMT026(p.UETR, CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS02",
		Assignor:      "MYBANKXXX",
		Assignee:      "THEIRBANK",
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (named): %v", err)
	}
	rep, err = ParseCAMT026([]byte(named.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026 (named): %v", err)
	}
	if rep.Assignor != "MYBANKXXX" || rep.Assignee != "THEIRBANK" {
		t.Errorf("assignor/assignee = %q/%q, want MYBANKXXX/THEIRBANK", rep.Assignor, rep.Assignee)
	}
	replay, err := l.ExportCAMT026(p.UETR, CAMT026ExportOptions{
		Justification: CAMT026JustificationMissingOrIncorrect,
		ReasonCode:    "MS02",
		Assignor:      "OTHERBANK",
		Assignee:      "SOMEBODYELSE",
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (replay): %v", err)
	}
	if !replay.Duplicate || replay.Message != named.Message {
		t.Error("a replay under different envelope names did not return the first rendering")
	}
}

// An export by EndToEndId works too: the payout resolves by ID,
// the transfer by entry ID — the inbound association order, with
// the UETR tried first.
func TestExportCAMT026_ByEndToEndID(t *testing.T) {
	l, _ := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-exp-8")
	res, err := l.ExportCAMT026(p.ID, CAMT026ExportOptions{
		Justification: CAMT026JustificationPossibleDuplicate,
	}, "bank-ops")
	if err != nil {
		t.Fatalf("ExportCAMT026 (payout by ID): %v", err)
	}
	if res.Kind != CAMT026AssociatedPayout || res.TargetID != p.ID {
		t.Errorf("association = %q/%q, want payout/po-026-exp-8", res.Kind, res.TargetID)
	}
	if res.UETR != p.UETR || res.EndToEndID != p.ID {
		t.Errorf("references = %q/%q, want %q/%q", res.UETR, res.EndToEndID, p.UETR, p.ID)
	}
	rep, err := ParseCAMT026([]byte(res.Message))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	if rep.UETR != p.UETR || rep.EndToEndID != p.ID {
		t.Errorf("round-trip references = %q/%q, want %q/%q", rep.UETR, rep.EndToEndID, p.UETR, p.ID)
	}
}
