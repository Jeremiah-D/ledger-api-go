package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// pacs004InboundDoc builds one inbound pacs.004.001.10 document for
// tests: the version banks send, not the .001.02 the LG-169 export
// emits — the parser must accept both.
func pacs004InboundDoc(t *testing.T, msgID, endToEndID, uetr, amount, ccy, reason string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.004.001.10">
<PmtRtr>
<GrpHdr><MsgId>%s</MsgId><CreDtTm>2026-10-25T02:00:00Z</CreDtTm></GrpHdr>
<OrgnlGrpInf><OrgnlMsgId>orig-msg-1</OrgnlMsgId><OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInf>
<TxInf>
<RtrdIntrBkSttlmAmt Ccy="%s">%s</RtrdIntrBkSttlmAmt>
<OrgnlEndToEndId>%s</OrgnlEndToEndId>
<OrgnlUETR>%s</OrgnlUETR>
<RtrRsnInf><Rsn><Prtry>%s</Prtry></Rsn></RtrRsnInf>
</TxInf>
</PmtRtr>
</Document>`, msgID, ccy, amount, endToEndID, uetr, reason))
}

func TestParsePacs004_HappyPath(t *testing.T) {
	ret, err := ParsePacs004(pacs004InboundDoc(t, "rtr-1", "po-ret-1", "uetr-ret-1", "2500.00", "USD", Pacs004ReturnDuplicate))
	if err != nil {
		t.Fatalf("ParsePacs004: %v", err)
	}
	if ret.MsgID != "rtr-1" || ret.OrgnlEndToEndID != "po-ret-1" || ret.OrgnlUETR != "uetr-ret-1" {
		t.Errorf("identity = %+v", ret)
	}
	if ret.ReturnedAmountCents != 250_000 || ret.Currency != "USD" {
		t.Errorf("amount = %d %s, want 250000 USD", ret.ReturnedAmountCents, ret.Currency)
	}
	if ret.ReasonCode != Pacs004ReturnDuplicate {
		t.Errorf("reason = %q", ret.ReasonCode)
	}
	// A one-digit fraction pads right: "2500.5" is 250050 cents.
	ret, err = ParsePacs004(pacs004InboundDoc(t, "rtr-2", "po-ret-1", "u", "2500.5", "USD", Pacs004ReturnDuplicate))
	if err != nil {
		t.Fatalf("ParsePacs004 one-digit fraction: %v", err)
	}
	if ret.ReturnedAmountCents != 250_050 {
		t.Errorf("amount = %d, want 250050", ret.ReturnedAmountCents)
	}
}

func TestParsePacs004_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		doc  []byte
	}{
		{"malformed xml", []byte(`<Document><PmtRtr>`)},
		{"missing MsgId", pacs004InboundDoc(t, "", "po-x", "u", "1.00", "USD", Pacs004ReturnDuplicate)},
		{"missing original", pacs004InboundDoc(t, "r", "", "u", "1.00", "USD", Pacs004ReturnDuplicate)},
		{"bad amount", pacs004InboundDoc(t, "r", "po-x", "u", "12.345", "USD", Pacs004ReturnDuplicate)},
		{"negative amount", pacs004InboundDoc(t, "r", "po-x", "u", "-5.00", "USD", Pacs004ReturnDuplicate)},
		{"zero amount", pacs004InboundDoc(t, "r", "po-x", "u", "0.00", "USD", Pacs004ReturnDuplicate)},
		{"bad currency", pacs004InboundDoc(t, "r", "po-x", "u", "1.00", "usd", Pacs004ReturnDuplicate)},
		{"unknown reason", pacs004InboundDoc(t, "r", "po-x", "u", "1.00", "USD", "NOPE")},
		{"missing reason", pacs004InboundDoc(t, "r", "po-x", "u", "1.00", "USD", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePacs004(tc.doc); err == nil {
				t.Error("ParsePacs004 accepted a document it must refuse")
			} else if !errors.Is(err, ErrPacs004Parse) {
				t.Errorf("error = %v, want wrapped ErrPacs004Parse", err)
			}
		})
	}
}

// pacs004EntryByID scans the journal for one entry ID: the test
// helper for reversal-row assertions (the ledger exposes Entries(),
// not a single-entry getter).
func pacs004EntryByID(t *testing.T, l *Ledger, id string) (JournalEntry, bool) {
	t.Helper()
	for _, e := range l.Entries() {
		if e.ID == id {
			return e, true
		}
	}
	return JournalEntry{}, false
}

func TestApplyPacs004Return_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-ret-1", 250_000)

	ret, err := ParsePacs004(pacs004InboundDoc(t, "rtr-1", p.ID, p.UETR, "2500.00", "USD", Pacs004ReturnCustomerRequest))
	if err != nil {
		t.Fatalf("ParsePacs004: %v", err)
	}
	res, err := l.ApplyPacs004Return(ret, "operator-1")
	if err != nil {
		t.Fatalf("ApplyPacs004Return: %v", err)
	}
	if !res.Applied || res.Duplicate {
		t.Errorf("result = %+v, want applied, not duplicate", res)
	}
	if res.ReversalEntryID != payoutReturnEntryID(p.ID) {
		t.Errorf("reversal entry = %q, want %q", res.ReversalEntryID, payoutReturnEntryID(p.ID))
	}
	got, ok := l.GetPayout(p.ID)
	if !ok {
		t.Fatal("payout vanished")
	}
	if got.Status != PayoutReturned {
		t.Errorf("status = %q, want returned", got.Status)
	}
	if got.ReturnEntryID != res.ReversalEntryID {
		t.Errorf("ReturnEntryID = %q, want %q", got.ReturnEntryID, res.ReversalEntryID)
	}
	// The reversal row exists with the settle leg mirrored: the
	// submit leg was debit clearing / credit payee, the settle leg
	// credit clearing / debit payee, so the return leg (debit
	// clearing / credit payee) restores the submit-side position.
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
}

func TestApplyPacs004Return_DuplicateNoOp(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-ret-2", 100_000)

	ret, err := ParsePacs004(pacs004InboundDoc(t, "rtr-dup", p.ID, p.UETR, "1000.00", "USD", Pacs004ReturnDuplicate))
	if err != nil {
		t.Fatalf("ParsePacs004: %v", err)
	}
	first, err := l.ApplyPacs004Return(ret, "op")
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := l.ApplyPacs004Return(ret, "op")
	if err != nil {
		t.Fatalf("second apply must be a no-op success, got: %v", err)
	}
	if !second.Duplicate || second.Applied {
		t.Errorf("second result = %+v, want duplicate, not applied", second)
	}
	if second.ReversalEntryID != first.ReversalEntryID {
		t.Errorf("duplicate reversal = %q, want original %q", second.ReversalEntryID, first.ReversalEntryID)
	}
	// No second row: exactly one reversal entry exists despite two
	// applies — the deterministic entry ID replayed, not
	// double-booked.
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
	if got.Status != PayoutReturned {
		t.Errorf("status = %q, want returned", got.Status)
	}
}

func TestApplyPacs004Return_AlreadyReturnedOtherMsgID(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-ret-3", 100_000)

	first, _ := ParsePacs004(pacs004InboundDoc(t, "rtr-a", p.ID, p.UETR, "1000.00", "USD", Pacs004ReturnDuplicate))
	if _, err := l.ApplyPacs004Return(first, "op"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, _ := ParsePacs004(pacs004InboundDoc(t, "rtr-b", p.ID, p.UETR, "1000.00", "USD", Pacs004ReturnDuplicate))
	res, err := l.ApplyPacs004Return(second, "op")
	if !errors.Is(err, ErrPacs004AlreadyReturned) {
		t.Errorf("error = %v, want ErrPacs004AlreadyReturned", err)
	}
	if res.RepairID != "" {
		t.Errorf("already-returned must not park a repair record, got %q", res.RepairID)
	}
}

func TestApplyPacs004Return_UnsettledRefused(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-ret-4", 100_000)

	ret, _ := ParsePacs004(pacs004InboundDoc(t, "rtr-u", p.ID, p.UETR, "1000.00", "USD", Pacs004ReturnDuplicate))
	res, err := l.ApplyPacs004Return(ret, "op")
	if !errors.Is(err, ErrPacs004OriginalNotSettled) {
		t.Errorf("error = %v, want ErrPacs004OriginalNotSettled", err)
	}
	if res.RepairID != "" {
		t.Errorf("unsettled must not park a repair record, got %q", res.RepairID)
	}
}

func TestApplyPacs004Return_UnknownOriginalParked(t *testing.T) {
	l := seedPayoutLedger(t)

	ret, _ := ParsePacs004(pacs004InboundDoc(t, "rtr-unk", "po-ghost", "u-ghost", "1000.00", "USD", Pacs004ReturnInvalidAccount))
	res, err := l.ApplyPacs004Return(ret, "op")
	if !errors.Is(err, ErrPacs004UnknownOriginal) {
		t.Errorf("error = %v, want ErrPacs004UnknownOriginal", err)
	}
	if res.RepairID == "" {
		t.Fatal("unknown original must park a repair record")
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
	// Parked returns are visibility, not resubmittable: the empty
	// Maker guarantees the STP gate can never pass them, so
	// resubmitting must fail and leave the record open.
	if _, err := l.RepairAndResubmit(res.RepairID, map[string]string{}, "op"); err == nil {
		t.Error("resubmitting a parked return must fail")
	}
	rec, _ = l.GetRepair(res.RepairID)
	if rec.Status != RepairStatusOpen {
		t.Errorf("repair status after failed resubmit = %q, want open", rec.Status)
	}
}

func TestApplyPacs004Return_AmountMismatchParked(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-ret-5", 100_000)

	// Partial return: 600 of 1000 settled.
	ret, _ := ParsePacs004(pacs004InboundDoc(t, "rtr-part", p.ID, p.UETR, "600.00", "USD", Pacs004ReturnWrongAmount))
	res, err := l.ApplyPacs004Return(ret, "op")
	if !errors.Is(err, ErrPacs004AmountMismatch) {
		t.Errorf("error = %v, want ErrPacs004AmountMismatch", err)
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
	ret2, _ := ParsePacs004(pacs004InboundDoc(t, "rtr-ccy", p.ID, p.UETR, "1000.00", "EUR", Pacs004ReturnWrongAmount))
	res2, err := l.ApplyPacs004Return(ret2, "op")
	if !errors.Is(err, ErrPacs004AmountMismatch) {
		t.Errorf("currency mismatch error = %v, want ErrPacs004AmountMismatch", err)
	}
	if res2.RepairID == "" {
		t.Fatal("currency mismatch must park a repair record")
	}
	// Nothing booked: the payout is still settled, no return row.
	got, _ := l.GetPayout(p.ID)
	if got.Status != PayoutSettled {
		t.Errorf("status = %q, want still settled", got.Status)
	}
	if _, ok := pacs004EntryByID(t, l, payoutReturnEntryID(p.ID)); ok {
		t.Error("mismatched return booked a reversal row")
	}
}

func TestApplyPacs004Return_RoundTripsOwnExport(t *testing.T) {
	// The return the ledger itself emits (LG-169) must parse and
	// apply: OrgnlEndToEndId = payout ID, amount, currency, reason
	// all round-trip through ParsePacs004.
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-ret-6", 250_000)

	doc, err := l.ExportPacs004(p.UETR, Pacs004ReturnDuplicate)
	if err != nil {
		t.Fatalf("ExportPacs004: %v", err)
	}
	// The export is .001.02; rewrite the namespace to the .001.10
	// banks send, proving the parser accepts both versions.
	doc10 := []byte(strings.Replace(string(doc),
		"pacs.004.001.02", "pacs.004.001.10", 1))
	ret, err := ParsePacs004(doc10)
	if err != nil {
		t.Fatalf("ParsePacs004(export): %v", err)
	}
	if ret.OrgnlEndToEndID != p.ID || ret.ReturnedAmountCents != p.AmountCents || ret.Currency != p.Currency {
		t.Errorf("round-trip = %+v, want payout %s %d %s", ret, p.ID, p.AmountCents, p.Currency)
	}
	res, err := l.ApplyPacs004Return(ret, "op")
	if err != nil {
		t.Fatalf("ApplyPacs004Return(export): %v", err)
	}
	if !res.Applied {
		t.Errorf("result = %+v, want applied", res)
	}
}
