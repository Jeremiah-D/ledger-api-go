package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pacs007TestDoc renders a pacs.007.001.02 FIToFIPmtRvsl answering
// orgnlMsgID (a payout ID) for uetr, reversing amount in ccy for
// reason, with the OrgnlTxRef amount/currency carried separately so
// a disagreeing reference can be built.
func pacs007TestDoc(t *testing.T, msgID, orgnlMsgID, msgNmID, uetr, amount, ccy, refAmount, refCcy, reason string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.007.001.02">`)
	sb.WriteString(`<FIToFIPmtRvsl><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInf><OrgnlMsgId>` + orgnlMsgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>` + msgNmID + `</OrgnlMsgNmId></OrgnlGrpInf>`)
	sb.WriteString(`<TxInf><RvslId>` + msgID + `</RvslId>`)
	sb.WriteString(`<OrgnlEndToEndId>` + orgnlMsgID + `</OrgnlEndToEndId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<RvsdIntrBkSttlmAmt Ccy="` + ccy + `">` + amount + `</RvsdIntrBkSttlmAmt>`)
	sb.WriteString(`<RtrRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></RtrRsnInf>`)
	sb.WriteString(`<OrgnlTxRef><OrgnlIntrBkSttlmAmt Ccy="` + refCcy + `">` + refAmount + `</OrgnlIntrBkSttlmAmt>`)
	sb.WriteString(`<OrgnlEndToEndId>` + orgnlMsgID + `</OrgnlEndToEndId></OrgnlTxRef>`)
	sb.WriteString(`</TxInf></FIToFIPmtRvsl></Document>`)
	return []byte(sb.String())
}

// pacs007TestReversal parses doc and applies it, failing the test on
// either error: the happy-path funnel for the apply tests.
func pacs007TestReversal(t *testing.T, l *Ledger, doc []byte, actor string) Pacs007ReversalResult {
	t.Helper()
	rvsl, err := ParsePacs007(doc)
	if err != nil {
		t.Fatalf("ParsePacs007: %v", err)
	}
	res, err := l.ApplyPacs007Reversal(rvsl, actor)
	if err != nil {
		t.Fatalf("ApplyPacs007Reversal: %v", err)
	}
	return res
}

// pacs007ReversalEvents reads the audit log and returns the
// pacs007_reversal_applied events in order. The log is closed first,
// flushing the queue, so this is the last thing a test does with
// that ledger.
func pacs007ReversalEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "pacs007_reversal_applied" {
			out = append(out, ev)
		}
	}
	return out
}

// The parser accepts the pacs.007.001.02 document shape: the
// namespace, the FIToFIPmtRvsl root, the pacs.008.001.02 message
// name, the payout ID as OrgnlMsgId, and the reversed amount.
func TestParsePacs007_Valid(t *testing.T) {
	doc := pacs007TestDoc(t, "rvsl-1", "po-pacs007-apply-1", pacs002OrgnlMsgName, "uetr-1", "2500.00", "USD", "2500.00", "USD", Pacs007ReversalDuplicate)
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.007.001.02"`,
		"<FIToFIPmtRvsl>",
		"<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId>",
		"<OrgnlMsgId>po-pacs007-apply-1</OrgnlMsgId>",
		`<RvsdIntrBkSttlmAmt Ccy="USD">2500.00</RvsdIntrBkSttlmAmt>`,
	} {
		if !strings.Contains(string(doc), want) {
			t.Fatalf("test doc missing %q", want)
		}
	}
	rvsl, err := ParsePacs007(doc)
	if err != nil {
		t.Fatalf("ParsePacs007: %v", err)
	}
	if rvsl.MsgID != "rvsl-1" {
		t.Errorf("MsgId = %q, want rvsl-1", rvsl.MsgID)
	}
	if rvsl.OrgnlMsgID != "po-pacs007-apply-1" {
		t.Errorf("OrgnlMsgId = %q, want po-pacs007-apply-1", rvsl.OrgnlMsgID)
	}
	if rvsl.OrgnlMsgNmID != "pacs.008.001.02" {
		t.Errorf("OrgnlMsgNmId = %q, want pacs.008.001.02", rvsl.OrgnlMsgNmID)
	}
	if rvsl.UETR != "uetr-1" {
		t.Errorf("UETR = %q, want uetr-1", rvsl.UETR)
	}
	if rvsl.ReversedAmountCents != 250_000 || rvsl.Currency != "USD" {
		t.Errorf("reversed amount = %d %s, want 250000 USD", rvsl.ReversedAmountCents, rvsl.Currency)
	}
	if rvsl.OrgnlTxRefAmountCents != 250_000 || rvsl.OrgnlTxRefCurrency != "USD" {
		t.Errorf("OrgnlTxRef amount = %d %s, want 250000 USD", rvsl.OrgnlTxRefAmountCents, rvsl.OrgnlTxRefCurrency)
	}
	if rvsl.ReasonCode != Pacs007ReversalDuplicate {
		t.Errorf("reason = %q, want DUPL", rvsl.ReasonCode)
	}
}

// Each admitted reversal reason parses: DUPL, AM09, FRAD, CUST.
func TestParsePacs007_ReasonCodes(t *testing.T) {
	for _, reason := range []string{
		Pacs007ReversalDuplicate,
		Pacs007ReversalWrongAmount,
		Pacs007ReversalFraudulent,
		Pacs007ReversalCustomerRequest,
	} {
		doc := pacs007TestDoc(t, "rvsl-"+reason, "po-pacs007-apply-1", pacs002OrgnlMsgName, "uetr-1", "2500.00", "USD", "2500.00", "USD", reason)
		rvsl, err := ParsePacs007(doc)
		if err != nil {
			t.Errorf("reason %q: ParsePacs007: %v", reason, err)
			continue
		}
		if rvsl.ReasonCode != reason {
			t.Errorf("reason %q: parsed as %q", reason, rvsl.ReasonCode)
		}
	}
}

// Malformed XML, an empty document, a missing MsgId, a missing
// original, a bad amount, a bad currency, and a reason outside the
// admitted vocabulary are all parse refusals — never a silent
// accept. A reversal answering another message family is refused
// with its own sentinel.
func TestParsePacs007_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		doc  []byte
		want error
	}{
		{"malformed xml", []byte(`<Document><FIToFIPmtRvsl>`), ErrPacs007Parse},
		{"empty document", nil, ErrPacs007Parse},
		{"missing MsgId", pacs007TestDoc(t, "", "po-x", pacs002OrgnlMsgName, "u", "1.00", "USD", "1.00", "USD", Pacs007ReversalDuplicate), ErrPacs007Parse},
		{"missing original", pacs007TestDoc(t, "r", "", pacs002OrgnlMsgName, "u", "1.00", "USD", "1.00", "USD", Pacs007ReversalDuplicate), ErrPacs007Parse},
		{"wrong message name", pacs007TestDoc(t, "r", "po-x", "pain.001.001.03", "u", "1.00", "USD", "1.00", "USD", Pacs007ReversalDuplicate), ErrPacs007UnexpectedMsgName},
		{"missing UETR", pacs007TestDoc(t, "r", "po-x", pacs002OrgnlMsgName, "", "1.00", "USD", "1.00", "USD", Pacs007ReversalDuplicate), ErrPacs007Parse},
		{"bad amount", pacs007TestDoc(t, "r", "po-x", pacs002OrgnlMsgName, "u", "12.345", "USD", "12.345", "USD", Pacs007ReversalDuplicate), ErrPacs007Parse},
		{"zero amount", pacs007TestDoc(t, "r", "po-x", pacs002OrgnlMsgName, "u", "0.00", "USD", "0.00", "USD", Pacs007ReversalDuplicate), ErrPacs007Parse},
		{"bad currency", pacs007TestDoc(t, "r", "po-x", pacs002OrgnlMsgName, "u", "1.00", "usd", "1.00", "usd", Pacs007ReversalDuplicate), ErrPacs007Parse},
		{"unknown reason", pacs007TestDoc(t, "r", "po-x", pacs002OrgnlMsgName, "u", "1.00", "USD", "1.00", "USD", "NOPE"), ErrPacs007Parse},
		{"missing reason", pacs007TestDoc(t, "r", "po-x", pacs002OrgnlMsgName, "u", "1.00", "USD", "1.00", "USD", ""), ErrPacs007Parse},
		{"missing ref amount", pacs007TestDoc(t, "r", "po-x", pacs002OrgnlMsgName, "u", "1.00", "USD", "", "USD", Pacs007ReversalDuplicate), ErrPacs007Parse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePacs007(tc.doc); err == nil {
				t.Error("ParsePacs007 accepted a document it must refuse")
			} else if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want wrapped %v", err, tc.want)
			}
		})
	}
}

// The reversal the ledger itself emits (LG-198) parses back: the
// export and the inbound parser agree on every field the apply path
// needs.
func TestParsePacs007_RoundTripsOwnExport(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-apply-2", 250_000)

	doc, err := l.ExportPacs007(p.UETR, Pacs007ReversalFraudulent)
	if err != nil {
		t.Fatalf("ExportPacs007: %v", err)
	}
	rvsl, err := ParsePacs007(doc)
	if err != nil {
		t.Fatalf("ParsePacs007(export): %v", err)
	}
	if rvsl.MsgID != pacs007MsgID(p.UETR) {
		t.Errorf("MsgId = %q, want %q", rvsl.MsgID, pacs007MsgID(p.UETR))
	}
	if rvsl.OrgnlMsgID != p.ID || rvsl.UETR != p.UETR {
		t.Errorf("identity = %+v, want payout %s UETR %s", rvsl, p.ID, p.UETR)
	}
	if rvsl.ReversedAmountCents != p.AmountCents || rvsl.Currency != p.Currency {
		t.Errorf("amount = %d %s, want %d %s", rvsl.ReversedAmountCents, rvsl.Currency, p.AmountCents, p.Currency)
	}
	if rvsl.OrgnlTxRefAmountCents != p.AmountCents || rvsl.OrgnlTxRefCurrency != p.Currency {
		t.Errorf("OrgnlTxRef = %d %s, want %d %s", rvsl.OrgnlTxRefAmountCents, rvsl.OrgnlTxRefCurrency, p.AmountCents, p.Currency)
	}
	if rvsl.ReasonCode != Pacs007ReversalFraudulent {
		t.Errorf("reason = %q, want FRAD", rvsl.ReasonCode)
	}
}

// Happy path: a settled payout reverses — status settled -> reversed,
// the deterministic reversal row books debit clearing / credit
// payee, ReversalEntryID lands on the instruction, the audit event
// carries the reason verbatim, and the gpi track does not move.
func TestApplyPacs007Reversal_HappyPath(t *testing.T) {
	al := mustAuditLog(t)
	l := seedPayoutLedger(t, WithAuditLog(al))
	p := settlePayoutForReturn(t, l, "po-pacs007-apply-3", 250_000)

	doc := pacs007TestDoc(t, "rvsl-happy", p.ID, pacs002OrgnlMsgName, p.UETR, "2500.00", "USD", "2500.00", "USD", Pacs007ReversalDuplicate)
	res := pacs007TestReversal(t, l, doc, "operator-1")
	if !res.Applied || res.Duplicate {
		t.Errorf("result = %+v, want applied, not duplicate", res)
	}
	if res.PayoutID != p.ID {
		t.Errorf("payout = %q, want %q", res.PayoutID, p.ID)
	}
	if res.ReversalEntryID != payoutReversalEntryID(p.ID) {
		t.Errorf("reversal entry = %q, want %q", res.ReversalEntryID, payoutReversalEntryID(p.ID))
	}
	got, ok := l.GetPayout(p.ID)
	if !ok {
		t.Fatal("payout vanished")
	}
	if got.Status != PayoutReversed {
		t.Errorf("status = %q, want reversed", got.Status)
	}
	if got.ReversalEntryID != res.ReversalEntryID {
		t.Errorf("ReversalEntryID = %q, want %q", got.ReversalEntryID, res.ReversalEntryID)
	}
	entry, ok := pacs004EntryByID(t, l, res.ReversalEntryID)
	if !ok {
		t.Fatalf("reversal entry %q not found", res.ReversalEntryID)
	}
	if entry.DebitAccount != "payout-clearing" || entry.CreditAccount != "saver" {
		t.Errorf("reversal legs = %s/%s, want payout-clearing/saver", entry.DebitAccount, entry.CreditAccount)
	}
	if entry.AmountCents != 250_000 {
		t.Errorf("reversal amount = %d, want 250000", entry.AmountCents)
	}
	// The reversal moves the payout, never the track.
	track, _ := l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusTracked || len(track.History) != 0 {
		t.Errorf("track moved on reversal: status=%q history=%d", track.Status, len(track.History))
	}
	events := pacs007ReversalEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("pacs007_reversal_applied events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Details["reversal_reason"] != "DUPL" {
		t.Errorf("audit reversal_reason = %v, want DUPL", ev.Details["reversal_reason"])
	}
	if ev.Details["payout_id"] != p.ID {
		t.Errorf("audit payout_id = %v, want %q", ev.Details["payout_id"], p.ID)
	}
	if ev.Details["reversal_entry_id"] != res.ReversalEntryID {
		t.Errorf("audit reversal_entry_id = %v, want %q", ev.Details["reversal_entry_id"], res.ReversalEntryID)
	}
	// Audit details round-trip through JSON, so the amount reads
	// back as a float.
	if ev.Details["amount_cents"] != float64(250_000) {
		t.Errorf("audit amount_cents = %v, want 250000", ev.Details["amount_cents"])
	}
}

// Re-applying the same MsgId is a no-op success: Duplicate set, the
// original reversal entry returned, exactly one reversal row.
func TestApplyPacs007Reversal_DuplicateNoOp(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-apply-4", 100_000)

	doc := pacs007TestDoc(t, "rvsl-dup", p.ID, pacs002OrgnlMsgName, p.UETR, "1000.00", "USD", "1000.00", "USD", Pacs007ReversalCustomerRequest)
	first := pacs007TestReversal(t, l, doc, "op")
	rvsl, err := ParsePacs007(doc)
	if err != nil {
		t.Fatalf("ParsePacs007: %v", err)
	}
	second, err := l.ApplyPacs007Reversal(rvsl, "op")
	if err != nil {
		t.Fatalf("second apply must be a no-op success, got: %v", err)
	}
	if !second.Duplicate || second.Applied {
		t.Errorf("second result = %+v, want duplicate, not applied", second)
	}
	if second.ReversalEntryID != first.ReversalEntryID {
		t.Errorf("duplicate reversal = %q, want original %q", second.ReversalEntryID, first.ReversalEntryID)
	}
	rows := 0
	for _, e := range l.Entries() {
		if e.ID == first.ReversalEntryID {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("reversal rows = %d, want exactly 1", rows)
	}
	got, _ := l.GetPayout(p.ID)
	if got.Status != PayoutReversed {
		t.Errorf("status = %q, want reversed", got.Status)
	}
}

// A second MsgId for an already-reversed payout is refused: the
// payment is undone, a second reversal has nothing left to reverse.
func TestApplyPacs007Reversal_AlreadyReversedOtherMsgID(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-apply-5", 100_000)

	first := pacs007TestDoc(t, "rvsl-a", p.ID, pacs002OrgnlMsgName, p.UETR, "1000.00", "USD", "1000.00", "USD", Pacs007ReversalDuplicate)
	pacs007TestReversal(t, l, first, "op")
	second, _ := ParsePacs007(pacs007TestDoc(t, "rvsl-b", p.ID, pacs002OrgnlMsgName, p.UETR, "1000.00", "USD", "1000.00", "USD", Pacs007ReversalDuplicate))
	res, err := l.ApplyPacs007Reversal(second, "op")
	if !errors.Is(err, ErrPacs007AlreadyReversed) {
		t.Errorf("error = %v, want ErrPacs007AlreadyReversed", err)
	}
	if res.RepairID != "" {
		t.Errorf("already-reversed must not park a repair record, got %q", res.RepairID)
	}
}

// An unsettled payout cannot be reversed: the money never left.
func TestApplyPacs007Reversal_UnsettledRefused(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs007-apply-6", 100_000)

	rvsl, _ := ParsePacs007(pacs007TestDoc(t, "rvsl-u", p.ID, pacs002OrgnlMsgName, p.UETR, "1000.00", "USD", "1000.00", "USD", Pacs007ReversalDuplicate))
	res, err := l.ApplyPacs007Reversal(rvsl, "op")
	if !errors.Is(err, ErrPacs007OriginalNotSettled) {
		t.Errorf("error = %v, want ErrPacs007OriginalNotSettled", err)
	}
	if res.RepairID != "" {
		t.Errorf("unsettled must not park a repair record, got %q", res.RepairID)
	}
}

// An unknown UETR parks the reversal in the repair queue: the bank
// is reversing a payment the ledger never tracked.
func TestApplyPacs007Reversal_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)

	rvsl, _ := ParsePacs007(pacs007TestDoc(t, "rvsl-unk", "po-ghost", pacs002OrgnlMsgName, "u-ghost", "1000.00", "USD", "1000.00", "USD", Pacs007ReversalFraudulent))
	res, err := l.ApplyPacs007Reversal(rvsl, "op")
	if !errors.Is(err, ErrGPIUnknownUETR) {
		t.Errorf("error = %v, want ErrGPIUnknownUETR", err)
	}
	if res.RepairID == "" {
		t.Fatal("unknown UETR must park a repair record")
	}
	rec, ok := l.GetRepair(res.RepairID)
	if !ok {
		t.Fatalf("repair record %q not found", res.RepairID)
	}
	if rec.Status != RepairStatusOpen {
		t.Errorf("repair status = %q, want open", rec.Status)
	}
	if rec.ReasonCode != RepairReasonUnknownRoute {
		t.Errorf("repair reason = %q, want unknown_route", rec.ReasonCode)
	}
	// Parked reversals are visibility, not resubmittable: the empty
	// Maker guarantees the STP gate can never pass them.
	if _, err := l.RepairAndResubmit(res.RepairID, map[string]string{}, "op"); err == nil {
		t.Error("resubmitting a parked reversal must fail")
	}
}

// A partial reversal, a currency mismatch, and a disagreeing
// OrgnlTxRef all park: a full reversal is the only reversal the
// ledger books on its own.
func TestApplyPacs007Reversal_AmountMismatchParked(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-apply-7", 100_000)

	// Partial reversal: 600 of 1000 settled.
	rvsl, _ := ParsePacs007(pacs007TestDoc(t, "rvsl-part", p.ID, pacs002OrgnlMsgName, p.UETR, "600.00", "USD", "600.00", "USD", Pacs007ReversalWrongAmount))
	res, err := l.ApplyPacs007Reversal(rvsl, "op")
	if !errors.Is(err, ErrPacs007AmountMismatch) {
		t.Errorf("error = %v, want ErrPacs007AmountMismatch", err)
	}
	if res.RepairID == "" {
		t.Fatal("amount mismatch must park a repair record")
	}
	rec, ok := l.GetRepair(res.RepairID)
	if !ok {
		t.Fatalf("repair record %q not found", res.RepairID)
	}
	if rec.ReasonCode != RepairReasonInvalidFormat {
		t.Errorf("repair reason = %q, want invalid_format", rec.ReasonCode)
	}
	// Currency mismatch parks the same way.
	rvsl2, _ := ParsePacs007(pacs007TestDoc(t, "rvsl-ccy", p.ID, pacs002OrgnlMsgName, p.UETR, "1000.00", "EUR", "1000.00", "EUR", Pacs007ReversalWrongAmount))
	res2, err := l.ApplyPacs007Reversal(rvsl2, "op")
	if !errors.Is(err, ErrPacs007AmountMismatch) {
		t.Errorf("currency mismatch error = %v, want ErrPacs007AmountMismatch", err)
	}
	if res2.RepairID == "" {
		t.Fatal("currency mismatch must park a repair record")
	}
	// A reversed amount that matches but an OrgnlTxRef that does
	// not parks too: the two amounts must agree with each other as
	// well as with the settled payout.
	rvsl3, _ := ParsePacs007(pacs007TestDoc(t, "rvsl-ref", p.ID, pacs002OrgnlMsgName, p.UETR, "1000.00", "USD", "600.00", "USD", Pacs007ReversalWrongAmount))
	res3, err := l.ApplyPacs007Reversal(rvsl3, "op")
	if !errors.Is(err, ErrPacs007AmountMismatch) {
		t.Errorf("OrgnlTxRef mismatch error = %v, want ErrPacs007AmountMismatch", err)
	}
	if res3.RepairID == "" {
		t.Fatal("OrgnlTxRef mismatch must park a repair record")
	}
	// Nothing booked: the payout is still settled, no reversal row.
	got, _ := l.GetPayout(p.ID)
	if got.Status != PayoutSettled {
		t.Errorf("status = %q, want still settled", got.Status)
	}
	if _, ok := pacs004EntryByID(t, l, payoutReversalEntryID(p.ID)); ok {
		t.Error("mismatched reversal booked a reversal row")
	}
}

// An OrgnlMsgId naming another payout is foreign: the UETR is the
// join key, the MsgId is the cross-check, and a mismatch refuses the
// reversal without touching the payout.
func TestApplyPacs007Reversal_ForeignMsgID(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-apply-8", 100_000)

	rvsl, err := ParsePacs007(pacs007TestDoc(t, "rvsl-foreign", "po-other-payout", pacs002OrgnlMsgName, p.UETR, "1000.00", "USD", "1000.00", "USD", Pacs007ReversalDuplicate))
	if err != nil {
		t.Fatalf("ParsePacs007: %v", err)
	}
	res, err := l.ApplyPacs007Reversal(rvsl, "op")
	if !errors.Is(err, ErrPacs007ForeignMsgID) {
		t.Errorf("error = %v, want ErrPacs007ForeignMsgID", err)
	}
	if res.RepairID != "" {
		t.Errorf("foreign MsgId must not park a repair record, got %q", res.RepairID)
	}
	got, _ := l.GetPayout(p.ID)
	if got.Status != PayoutSettled {
		t.Errorf("status = %q, want still settled", got.Status)
	}
}

// The reversal the ledger itself emits (LG-198) parses and applies
// end to end: export -> parse -> settled becomes reversed.
func TestApplyPacs007Reversal_RoundTripsOwnExport(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-apply-9", 250_000)

	doc, err := l.ExportPacs007(p.UETR, Pacs007ReversalCustomerRequest)
	if err != nil {
		t.Fatalf("ExportPacs007: %v", err)
	}
	res := pacs007TestReversal(t, l, doc, "op")
	if !res.Applied {
		t.Errorf("result = %+v, want applied", res)
	}
	got, _ := l.GetPayout(p.ID)
	if got.Status != PayoutReversed {
		t.Errorf("status = %q, want reversed", got.Status)
	}
	if got.ReversalEntryID == "" {
		t.Error("ReversalEntryID empty after round-trip apply")
	}
}
