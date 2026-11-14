package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// pain014Doc renders one inbound pain.014.001.07 debtor response:
// MsgId msgID answering the pain.013 message for endToEndID with
// TxSts txSts. amount/ccy state the OrgnlTxRef amount when amount
// is non-empty; accAmount/accCcy state the PmtCondSts accepted
// amount when accAmount is non-empty; reasonCd/reasonInfo state the
// StsRsnInf reason when reasonCd is non-empty.
func pain014Doc(msgID, endToEndID, txSts, amount, ccy, accAmount, accCcy, reasonCd, reasonInfo string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.014.001.07">`)
	sb.WriteString(`<CdtrPmtActvtnReqStsRpt><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T10:00:00Z</CreDtTm><InitgPty><Nm>DEBTORBANK</Nm></InitgPty></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInfAndSts><OrgnlMsgId>pain013-` + endToEndID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pain.013.001.07</OrgnlMsgNmId>`)
	sb.WriteString(`<OrgnlCreDtTm>2026-11-14T09:00:00Z</OrgnlCreDtTm><OrgnlNbOfTxs>1</OrgnlNbOfTxs></OrgnlGrpInfAndSts>`)
	sb.WriteString(`<OrgnlPmtInfAndSts><OrgnlPmtInfId>pain013-` + endToEndID + `</OrgnlPmtInfId><TxInfAndSts>`)
	sb.WriteString(`<OrgnlEndToEndId>` + endToEndID + `</OrgnlEndToEndId>`)
	sb.WriteString(`<TxSts>` + txSts + `</TxSts>`)
	if reasonCd != "" {
		sb.WriteString(`<StsRsnInf><Rsn><Cd>` + reasonCd + `</Cd></Rsn>`)
		if reasonInfo != "" {
			sb.WriteString(`<AddtlInf>` + reasonInfo + `</AddtlInf>`)
		}
		sb.WriteString(`</StsRsnInf>`)
	}
	if accAmount != "" {
		sb.WriteString(`<PmtCondSts><AccptdAmt Ccy="` + accCcy + `">` + accAmount + `</AccptdAmt>`)
		sb.WriteString(`<GrntedPmt>true</GrntedPmt><EarlyPmt>false</EarlyPmt></PmtCondSts>`)
	}
	sb.WriteString(`<OrgnlTxRef>`)
	if amount != "" {
		sb.WriteString(`<Amt><InstdAmt Ccy="` + ccy + `">` + amount + `</InstdAmt></Amt>`)
	}
	sb.WriteString(`<ReqdExctnDt><Dt>2026-11-14</Dt></ReqdExctnDt>`)
	sb.WriteString(`<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>`)
	sb.WriteString(`<Cdtr><Nm>merchant</Nm></Cdtr>`)
	sb.WriteString(`</OrgnlTxRef></TxInfAndSts></OrgnlPmtInfAndSts></CdtrPmtActvtnReqStsRpt></Document>`)
	return sb.String()
}

// pain014Fixture builds the pain013Fixture ledger (issued RfP
// rfp-013, merchant asks customer for USD 50.00, expiring in an
// hour) with an audit log attached, so the inbound audit marker can
// be asserted precisely.
func pain014Fixture(t *testing.T) (*Ledger, RfP, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	ctx := context.Background()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	r, dup, err := l.CreateRfP(ctx, RfP{
		ID:          "rfp-013",
		Creditor:    "merchant",
		Debtor:      "customer",
		AmountCents: 5000,
		Currency:    "USD",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil || dup {
		t.Fatalf("CreateRfP: %v dup=%v", err, dup)
	}
	return l, r, al
}

// Accept happy path: an ACCP response pays the RfP — issued -> paid
// through the full Post gate, with the deterministic payout transfer
// ID and memo, and the inbound audit marker on top of the RfP legs.
func TestApplyPain014Response_AcceptPays(t *testing.T) {
	l, _, al := pain014Fixture(t)
	doc := pain014Doc("pain014-accept-1", "rfp-013", "ACCP", "50.00", "USD", "", "", "", "")
	resp, err := ParsePain014([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain014: %v", err)
	}
	if resp.Response != Pain014ResponseAccepted || resp.TxStatus != "ACCP" {
		t.Fatalf("response = %q/%q, want accepted/ACCP", resp.Response, resp.TxStatus)
	}
	if !resp.HasAmount || resp.AmountCents != 5000 || resp.Currency != "USD" {
		t.Errorf("amount = %v %d %q, want has 5000 USD", resp.HasAmount, resp.AmountCents, resp.Currency)
	}
	out, err := l.ApplyPain014Response(resp, "debtor-bank")
	if err != nil {
		t.Fatalf("ApplyPain014Response: %v", err)
	}
	if out.Status != RfPStatusPaid {
		t.Errorf("status = %q, want paid", out.Status)
	}
	if out.TransferID != "rfp/rfp-013/payout" {
		t.Errorf("transfer_id = %q, want rfp/rfp-013/payout", out.TransferID)
	}
	// The full Post gate ran: the money moved debtor -> creditor.
	if got := l.BalanceIn("customer", "USD"); got != 10000-5000 {
		t.Errorf("customer balance = %d, want %d", got, 10000-5000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 5000 {
		t.Errorf("merchant balance = %d, want 5000", got)
	}
	// The payout transfer carries the RfP memo.
	foundMemo := false
	for _, e := range l.Entries() {
		if e.Memo == "rfp_of:rfp-013" {
			foundMemo = true
		}
	}
	if !foundMemo {
		t.Error("no entry carries the rfp_of:rfp-013 memo")
	}

	events := readAuditOps(t, al)
	if countOp(events, "pain014_response_applied") != 1 {
		t.Errorf("pain014_response_applied count = %d, want 1", countOp(events, "pain014_response_applied"))
	}
	if countOp(events, "rfp_accepted") != 1 || countOp(events, "rfp_paid") != 1 {
		t.Errorf("rfp_accepted/rfp_paid counts = %d/%d, want 1/1",
			countOp(events, "rfp_accepted"), countOp(events, "rfp_paid"))
	}
	ev := findOp(events, "pain014_response_applied")
	if ev == nil {
		t.Fatal("pain014_response_applied event missing")
	}
	if ev.TraceID != "pain014-accept-1" {
		t.Errorf("trace = %q, want pain014-accept-1", ev.TraceID)
	}
	if ev.Actor != "debtor-bank" {
		t.Errorf("actor = %q, want debtor-bank", ev.Actor)
	}
	if ev.Details["rfp_id"] != "rfp-013" || ev.Details["response"] != "accepted" {
		t.Errorf("details = %v, want rfp_id rfp-013 response accepted", ev.Details)
	}
	if ev.Details["transfer_id"] != "rfp/rfp-013/payout" {
		t.Errorf("details transfer_id = %v, want rfp/rfp-013/payout", ev.Details["transfer_id"])
	}
	if len(ev.EntryIDs) == 0 {
		t.Error("inbound audit event carries no entry IDs")
	}
}

// Reject happy path: an RJCT response rejects the RfP — issued ->
// rejected, no money moves, the bank's reason code rides the audit.
func TestApplyPain014Response_RejectRefuses(t *testing.T) {
	l, r, al := pain014Fixture(t)
	_ = r

	doc := pain014Doc("pain014-reject-1", "rfp-013", "RJCT", "50.00", "USD", "", "", "AM04", "Insufficient funds at debtor bank")
	resp, err := ParsePain014([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain014: %v", err)
	}
	if resp.Response != Pain014ResponseRejected || resp.ReasonCode != "AM04" || resp.ReasonInfo != "Insufficient funds at debtor bank" {
		t.Fatalf("response = %q reason %q/%q, want rejected AM04/<info>", resp.Response, resp.ReasonCode, resp.ReasonInfo)
	}
	out, err := l.ApplyPain014Response(resp, "debtor-bank")
	if err != nil {
		t.Fatalf("ApplyPain014Response: %v", err)
	}
	if out.Status != RfPStatusRejected {
		t.Errorf("status = %q, want rejected", out.Status)
	}
	if got := l.BalanceIn("customer", "USD"); got != 10000 {
		t.Errorf("customer balance = %d, want 10000 (no money moves on reject)", got)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 0 {
		t.Errorf("merchant balance = %d, want 0", got)
	}

	events := readAuditOps(t, al)
	if countOp(events, "pain014_response_applied") != 1 {
		t.Errorf("pain014_response_applied count = %d, want 1", countOp(events, "pain014_response_applied"))
	}
	if countOp(events, "rfp_rejected") != 1 {
		t.Errorf("rfp_rejected count = %d, want 1", countOp(events, "rfp_rejected"))
	}
	if countOp(events, "rfp_paid") != 0 {
		t.Error("a rejected response booked a payout")
	}
	ev := findOp(events, "pain014_response_applied")
	if ev == nil {
		t.Fatal("pain014_response_applied event missing")
	}
	if ev.Details["response"] != "rejected" || ev.Details["reason_code"] != "AM04" {
		t.Errorf("details = %v, want response rejected reason_code AM04", ev.Details)
	}
	if _, ok := ev.Details["transfer_id"]; ok {
		t.Errorf("reject audit carries a transfer_id: %v", ev.Details)
	}
}

// Unknown EndToEndId: a response to a request the ledger never
// issued is refused fail-closed.
func TestApplyPain014Response_UnknownEndToEndId(t *testing.T) {
	l, _, _ := pain014Fixture(t)
	resp, err := ParsePain014([]byte(pain014Doc("pain014-unk-1", "rfp-nope", "ACCP", "50.00", "USD", "", "", "", "")))
	if err != nil {
		t.Fatalf("ParsePain014: %v", err)
	}
	if _, err := l.ApplyPain014Response(resp, "debtor-bank"); !errors.Is(err, ErrRfPNotFound) {
		t.Errorf("apply unknown EndToEndId: %v, want ErrRfPNotFound", err)
	}
}

// Terminal RfPs refuse responses: paid, rejected, and expired (lazy)
// are all closed to a second answer.
func TestApplyPain014Response_TerminalRefusals(t *testing.T) {
	l, _, _ := pain014Fixture(t)
	ctx := context.Background()

	// Paid: accept once, then a distinct response is refused.
	resp, _ := ParsePain014([]byte(pain014Doc("pain014-t-1", "rfp-013", "ACCP", "50.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(resp, "debtor-bank"); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	resp2, _ := ParsePain014([]byte(pain014Doc("pain014-t-2", "rfp-013", "RJCT", "50.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(resp2, "debtor-bank"); !errors.Is(err, ErrRfPNotIssued) {
		t.Errorf("response to paid RfP: %v, want ErrRfPNotIssued", err)
	}

	// Rejected: reject once, then a distinct response is refused.
	r2, dup, err := l.CreateRfP(ctx, RfP{ID: "rfp-013b", Creditor: "merchant", Debtor: "customer",
		AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil || dup {
		t.Fatalf("CreateRfP rfp-013b: %v dup=%v", err, dup)
	}
	_ = r2
	resp3, _ := ParsePain014([]byte(pain014Doc("pain014-t-3", "rfp-013b", "RJCT", "10.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(resp3, "debtor-bank"); err != nil {
		t.Fatalf("first reject: %v", err)
	}
	resp4, _ := ParsePain014([]byte(pain014Doc("pain014-t-4", "rfp-013b", "ACCP", "10.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(resp4, "debtor-bank"); !errors.Is(err, ErrRfPNotIssued) {
		t.Errorf("response to rejected RfP: %v, want ErrRfPNotIssued", err)
	}

	// Expired: lazy expiry marks the record and refuses.
	r3, dup, err := l.CreateRfP(ctx, RfP{ID: "rfp-013c", Creditor: "merchant", Debtor: "customer",
		AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(50 * time.Millisecond)})
	if err != nil || dup {
		t.Fatalf("CreateRfP rfp-013c: %v dup=%v", err, dup)
	}
	_ = r3
	time.Sleep(60 * time.Millisecond)
	resp5, _ := ParsePain014([]byte(pain014Doc("pain014-t-5", "rfp-013c", "ACCP", "10.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(resp5, "debtor-bank"); !errors.Is(err, ErrRfPExpired) {
		t.Errorf("response to expired RfP: %v, want ErrRfPExpired", err)
	}
	if got, ok := l.GetRfP("rfp-013c"); !ok || got.Status != RfPStatusExpired {
		t.Errorf("expired RfP status = %q, want expired", got.Status)
	}
}

// Amount/currency gate: a response stating a different amount (in
// either amount element) or currency than the registered request is
// refused — it is evidence about a different payment.
func TestApplyPain014Response_AmountMismatch(t *testing.T) {
	l, _, _ := pain014Fixture(t)

	cases := []struct {
		name string
		doc  string
		want error
	}{
		{
			"original amount differs",
			pain014Doc("pain014-am-1", "rfp-013", "ACCP", "49.99", "USD", "", "", "", ""),
			ErrPain014AmountMismatch,
		},
		{
			"original currency differs",
			pain014Doc("pain014-am-2", "rfp-013", "ACCP", "50.00", "EUR", "", "", "", ""),
			ErrPain014CurrencyMismatch,
		},
		{
			"accepted amount differs",
			pain014Doc("pain014-am-3", "rfp-013", "ACCP", "50.00", "USD", "25.00", "USD", "", ""),
			ErrPain014AmountMismatch,
		},
		{
			"accepted currency differs",
			pain014Doc("pain014-am-4", "rfp-013", "ACCP", "50.00", "USD", "50.00", "EUR", "", ""),
			ErrPain014CurrencyMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ParsePain014([]byte(tc.doc))
			if err != nil {
				t.Fatalf("ParsePain014: %v", err)
			}
			if _, err := l.ApplyPain014Response(resp, "debtor-bank"); !errors.Is(err, tc.want) {
				t.Errorf("apply: %v, want %v", err, tc.want)
			}
		})
	}
	// Nothing booked by any refused response: the RfP is still
	// issued and the balances are untouched.
	if got, _ := l.GetRfP("rfp-013"); got.Status != RfPStatusIssued {
		t.Errorf("RfP status = %q, want issued after refused responses", got.Status)
	}
	if got := l.BalanceIn("customer", "USD"); got != 10000 {
		t.Errorf("customer balance = %d, want 10000", got)
	}
}

// Illegal documents are refused as a whole: the wrong schema
// version, a report answering a different message, a verdict outside
// ACCP/RJCT, a missing EndToEndId, malformed XML, and an unreadable
// amount never reach the apply.
func TestParsePain014_IllegalDocuments(t *testing.T) {
	good := pain014Doc("pain014-il-1", "rfp-013", "ACCP", "50.00", "USD", "", "", "", "")
	cases := []struct {
		name string
		doc  string
	}{
		{"empty document", ""},
		{"malformed XML", "<Document><broken"},
		{"wrong schema version", strings.Replace(good, "pain.014.001.07", "pain.014.001.05", 1)},
		{"wrong original message", strings.Replace(good, "pain.013.001.07", "pain.001.001.03", 1)},
		{"pending verdict", pain014Doc("pain014-il-2", "rfp-013", "PDNG", "50.00", "USD", "", "", "", "")},
		{"accepted-settlement verdict", pain014Doc("pain014-il-3", "rfp-013", "ACSP", "50.00", "USD", "", "", "", "")},
		{"missing EndToEndId", strings.Replace(good, "<OrgnlEndToEndId>rfp-013</OrgnlEndToEndId>", "", 1)},
		{"missing MsgId", strings.Replace(good, "<MsgId>pain014-il-1</MsgId>", "", 1)},
		{"unreadable amount", pain014Doc("pain014-il-4", "rfp-013", "ACCP", "fifty", "USD", "", "", "", "")},
		{"non-positive amount", pain014Doc("pain014-il-5", "rfp-013", "ACCP", "0.00", "USD", "", "", "", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePain014([]byte(tc.doc)); !errors.Is(err, ErrPain014Parse) {
				t.Errorf("ParsePain014: %v, want ErrPain014Parse", err)
			}
		})
	}
}

// Replay idempotency: the same MsgId re-applied returns the stored
// RfP — no second payout, no second inbound audit event.
func TestApplyPain014Response_ReplayIdempotent(t *testing.T) {
	l, _, al := pain014Fixture(t)

	doc := pain014Doc("pain014-replay-1", "rfp-013", "ACCP", "50.00", "USD", "", "", "", "")
	resp, err := ParsePain014([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain014: %v", err)
	}
	first, err := l.ApplyPain014Response(resp, "debtor-bank")
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	resp2, err := ParsePain014([]byte(doc))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	second, err := l.ApplyPain014Response(resp2, "debtor-bank")
	if err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	if second.Status != RfPStatusPaid || second.TransferID != first.TransferID {
		t.Errorf("replay = %+v, want the stored paid RfP", second)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 5000 {
		t.Errorf("merchant balance = %d, want 5000 (one payout only)", got)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain014_response_applied") != 1 {
		t.Errorf("pain014_response_applied count = %d, want 1", countOp(events, "pain014_response_applied"))
	}
	if countOp(events, "rfp_paid") != 1 {
		t.Errorf("rfp_paid count = %d, want 1", countOp(events, "rfp_paid"))
	}

	// The same holds for a rejected replay.
	l2, _, al2 := pain014Fixture(t)
	docR := pain014Doc("pain014-replay-2", "rfp-013", "RJCT", "50.00", "USD", "", "", "AM04", "")
	r1, err := ParsePain014([]byte(docR))
	if err != nil {
		t.Fatalf("ParsePain014: %v", err)
	}
	if _, err := l2.ApplyPain014Response(r1, "debtor-bank"); err != nil {
		t.Fatalf("first reject: %v", err)
	}
	r2, err := ParsePain014([]byte(docR))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	out, err := l2.ApplyPain014Response(r2, "debtor-bank")
	if err != nil {
		t.Fatalf("replay reject: %v", err)
	}
	if out.Status != RfPStatusRejected {
		t.Errorf("replay status = %q, want rejected", out.Status)
	}
	if countOp(readAuditOps(t, al2), "pain014_response_applied") != 1 {
		t.Error("rejected replay emitted a second inbound audit event")
	}
}

// A MsgId reused for a different response is a conflict, not a
// replay: one message ID names one response.
func TestApplyPain014Response_MsgIDConflict(t *testing.T) {
	l, _, _ := pain014Fixture(t)

	resp, _ := ParsePain014([]byte(pain014Doc("pain014-cf-1", "rfp-013", "ACCP", "50.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(resp, "debtor-bank"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Same MsgId, opposite verdict.
	other, _ := ParsePain014([]byte(pain014Doc("pain014-cf-1", "rfp-013", "RJCT", "50.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(other, "debtor-bank"); !errors.Is(err, ErrPain014MsgIDConflict) {
		t.Errorf("conflicting reuse: %v, want ErrPain014MsgIDConflict", err)
	}
}

// A failed accept books nothing and records nothing: when the
// debtor's account is frozen, the payout cannot post, the RfP stays
// issued, and the same response still applies once the account is
// unfrozen.
func TestApplyPain014Response_FailedAcceptNotRecorded(t *testing.T) {
	l, _, _ := pain014Fixture(t)

	// Freeze the debtor: the payout gate refuses.
	l.Freeze("customer")
	resp, _ := ParsePain014([]byte(pain014Doc("pain014-fail-1", "rfp-013", "ACCP", "50.00", "USD", "", "", "", "")))
	if _, err := l.ApplyPain014Response(resp, "debtor-bank"); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("accept on frozen debtor: %v, want ErrAccountFrozen", err)
	}
	if got, _ := l.GetRfP("rfp-013"); got.Status != RfPStatusIssued {
		t.Errorf("RfP status = %q, want issued after failed accept", got.Status)
	}
	// Unfreeze and retry the same document: it applies.
	l.Unfreeze("customer")
	resp2, _ := ParsePain014([]byte(pain014Doc("pain014-fail-1", "rfp-013", "ACCP", "50.00", "USD", "", "", "", "")))
	out, err := l.ApplyPain014Response(resp2, "debtor-bank")
	if err != nil {
		t.Fatalf("retry after unfreeze: %v", err)
	}
	if out.Status != RfPStatusPaid {
		t.Errorf("retry status = %q, want paid", out.Status)
	}
}

// Parse carries the wire through: UETR is informational, the reason
// code prefers Cd over Prtry, and an amount-less response parses
// without the amount gate.
func TestParsePain014_Fields(t *testing.T) {
	doc := pain014Doc("pain014-f-1", "rfp-013", "RJCT", "", "USD", "", "", "AM04", "debtor refused")
	// Add a UETR and a Prtry reason to the document.
	doc = strings.Replace(doc, "<TxSts>RJCT</TxSts>",
		"<OrgnlUETR>9f2c8e1a-1111-4111-8111-111111111111</OrgnlUETR><TxSts>RJCT</TxSts>", 1)
	doc = strings.Replace(doc, "<Rsn><Cd>AM04</Cd></Rsn>",
		"<Rsn><Cd></Cd><Prtry>DEBTOR-REFUSED</Prtry></Rsn>", 1)
	resp, err := ParsePain014([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain014: %v", err)
	}
	if resp.UETR != "9f2c8e1a-1111-4111-8111-111111111111" {
		t.Errorf("UETR = %q", resp.UETR)
	}
	if resp.ReasonCode != "DEBTOR-REFUSED" {
		t.Errorf("reason code = %q, want DEBTOR-REFUSED (Prtry fallback when Cd is empty)", resp.ReasonCode)
	}
	if resp.HasAmount {
		t.Error("amount-less response reports HasAmount")
	}
	if resp.OrgnlMsgID != "pain013-rfp-013" || resp.OrgnlMsgNmID != "pain.013.001.07" {
		t.Errorf("original = %q/%q, want pain013-rfp-013/pain.013.001.07", resp.OrgnlMsgID, resp.OrgnlMsgNmID)
	}
}
