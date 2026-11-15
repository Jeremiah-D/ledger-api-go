package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pain007Doc renders one inbound pain.007.001.02 document of the
// given schema version: MsgId msgID cancelling the transfer named
// by endToEnd with the stated reason, original amount, currency,
// and OrgnlTxRef IBANs — the shape RenderPain007Request emits, so
// a document this ledger's export renders parses back.
func pain007Doc(version, msgID, endToEnd, reason, amount, ccy, debtorIBAN, creditorIBAN string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.007.` + version + `">`)
	sb.WriteString(`<CstmrPmtCxlReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm>`)
	sb.WriteString(`<InitgPty><Nm>LEDGER</Nm></InitgPty></GrpHdr>`)
	sb.WriteString(`<OrgnlPmtInfAndSts><TxInfAndSts>`)
	sb.WriteString(`<OrgnlEndToEndId>` + endToEnd + `</OrgnlEndToEndId>`)
	if reason != "" {
		sb.WriteString(`<CxlStsRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlStsRsnInf>`)
	}
	sb.WriteString(`<OrgnlTxRef><Amt><InstdAmt Ccy="` + ccy + `">` + amount + `</InstdAmt></Amt>`)
	sb.WriteString(`<DbtrAcct><Id><IBAN>` + debtorIBAN + `</IBAN></Id></DbtrAcct>`)
	sb.WriteString(`<CdtrAcct><Id><IBAN>` + creditorIBAN + `</IBAN></Id></CdtrAcct>`)
	sb.WriteString(`</OrgnlTxRef></TxInfAndSts></OrgnlPmtInfAndSts>`)
	sb.WriteString(`</CstmrPmtCxlReq></Document>`)
	return sb.String()
}

// parsePain007Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyPain007Doc(t *testing.T, l *Ledger, doc string) (Pain007ApplyResult, error) {
	t.Helper()
	req, err := ParsePain007([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain007: %v", err)
	}
	return l.ApplyPain007(req, "customer-bank")
}

func TestParsePain007ValidDocument(t *testing.T) {
	req, err := ParsePain007([]byte(pain007Doc("001.02", "msg-1", "pay-1", "CUST", "150.00", "USD", testIBANDE, testIBANGB)))
	if err != nil {
		t.Fatalf("ParsePain007: %v", err)
	}
	if req.MsgID != "msg-1" || req.OrgnlEndToEndID != "pay-1" || req.ReasonCode != "CUST" {
		t.Errorf("req = %+v, want msg-1/pay-1/CUST", req)
	}
	if req.AmountCents != 15000 || req.Currency != "USD" {
		t.Errorf("amount = %d %s, want 15000 USD", req.AmountCents, req.Currency)
	}
	if req.DebtorIBAN != testIBANDE || req.CreditorIBAN != testIBANGB {
		t.Errorf("IBANs = %q/%q, want %q/%q", req.DebtorIBAN, req.CreditorIBAN, testIBANDE, testIBANGB)
	}
	// No reason stated: the Rsn block is omitted, never defaulted.
	req2, err := ParsePain007([]byte(pain007Doc("001.02", "msg-2", "pay-1", "", "150.00", "USD", testIBANDE, testIBANGB)))
	if err != nil {
		t.Fatalf("ParsePain007 reasonless: %v", err)
	}
	if req2.ReasonCode != "" {
		t.Errorf("reasonless req.ReasonCode = %q, want empty", req2.ReasonCode)
	}
}

func TestParsePain007RoundTripsExport(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	doc, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1", ReasonCode: "CUST"})
	if err != nil {
		t.Fatalf("ExportPain007: %v", err)
	}
	req, err := ParsePain007(doc)
	if err != nil {
		t.Fatalf("ParsePain007(export): %v", err)
	}
	if req.MsgID != "pay-1/cxl" || req.OrgnlEndToEndID != "pay-1" || req.ReasonCode != "CUST" {
		t.Errorf("req = %+v, want pay-1/cxl/pay-1/CUST", req)
	}
	if req.AmountCents != 15000 || req.Currency != "USD" {
		t.Errorf("amount = %d %s, want 15000 USD", req.AmountCents, req.Currency)
	}
}

func TestParsePain007WrongNamespace(t *testing.T) {
	// Wrong schema version: the namespace gate refuses it, but the
	// document still names its MsgId, so it parks.
	doc := pain007Doc("001.01", "msg-bad", "pay-1", "CUST", "150.00", "USD", testIBANDE, testIBANGB)
	if _, err := ParsePain007([]byte(doc)); !errors.Is(err, ErrInvalidPain007) {
		t.Errorf("err = %v, want ErrInvalidPain007", err)
	}
	l := painTestLedger(t)
	repairID, err := l.ParkPain007Unparsed([]byte(doc), "ops")
	if err != nil {
		t.Fatalf("ParkPain007Unparsed: %v", err)
	}
	if repairID == "" {
		t.Error("repairID empty, want the parked record ID")
	}
	// Garbage bytes name nothing parkable: no record, no error.
	if repairID, err := l.ParkPain007Unparsed([]byte("not xml at all"), "ops"); err != nil || repairID != "" {
		t.Errorf("garbage: repairID=%q err=%v, want empty/nil", repairID, err)
	}
}

func TestParsePain007InvalidReason(t *testing.T) {
	doc := pain007Doc("001.02", "msg-1", "pay-1", "NOPE", "150.00", "USD", testIBANDE, testIBANGB)
	if _, err := ParsePain007([]byte(doc)); !errors.Is(err, ErrInvalidPain007Reason) {
		t.Errorf("err = %v, want ErrInvalidPain007Reason", err)
	}
}

func TestParsePain007MissingFields(t *testing.T) {
	// Missing OrgnlEndToEndId.
	doc := strings.Replace(pain007Doc("001.02", "msg-1", "pay-1", "CUST", "150.00", "USD", testIBANDE, testIBANGB),
		"<OrgnlEndToEndId>pay-1</OrgnlEndToEndId>", "", 1)
	if _, err := ParsePain007([]byte(doc)); !errors.Is(err, ErrInvalidPain007) {
		t.Errorf("missing EndToEndId: err = %v, want ErrInvalidPain007", err)
	}
	// Missing amount.
	doc = strings.Replace(pain007Doc("001.02", "msg-1", "pay-1", "CUST", "150.00", "USD", testIBANDE, testIBANGB),
		">150.00<", "><", 1)
	if _, err := ParsePain007([]byte(doc)); !errors.Is(err, ErrInvalidPain007) {
		t.Errorf("missing amount: err = %v, want ErrInvalidPain007", err)
	}
	// Non-positive amount.
	if _, err := ParsePain007([]byte(pain007Doc("001.02", "msg-1", "pay-1", "CUST", "0.00", "USD", testIBANDE, testIBANGB))); !errors.Is(err, ErrInvalidPain007) {
		t.Errorf("zero amount: err = %v, want ErrInvalidPain007", err)
	}
}

func TestApplyPain007UnknownTransfer(t *testing.T) {
	l := painTestLedger(t)
	res, err := applyPain007Doc(t, l, pain007Doc("001.02", "msg-1", "no-such", "CUST", "150.00", "USD", testIBANDE, testIBANGB))
	if !errors.Is(err, ErrPain007UnknownOriginal) {
		t.Fatalf("err = %v, want ErrPain007UnknownOriginal", err)
	}
	if res.RepairID == "" {
		t.Error("RepairID empty, want the parked repair record ID")
	}
}

func TestApplyPain007AmountMismatch(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	// The request names pay-1 but states a different amount: it
	// points at a different payment, so it fails closed.
	res, err := applyPain007Doc(t, l, pain007Doc("001.02", "msg-1", "pay-1", "AM09", "150.01", "USD", testIBANDE, testIBANGB))
	if !errors.Is(err, ErrPain007AmountMismatch) {
		t.Fatalf("err = %v, want ErrPain007AmountMismatch", err)
	}
	if res.RepairID == "" {
		t.Error("RepairID empty, want the parked repair record ID")
	}
	// Nothing was booked: no reversal entry exists.
	if _, taken := l.entries["pay-1/reversal"]; taken {
		t.Error("reversal entry exists after a refused cancellation")
	}
}

func TestApplyPain007Success(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, testBICDE)
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	before := l.Version()

	res, err := applyPain007Doc(t, l, pain007Doc("001.02", "msg-1", "pay-1", "CUST", "150.00", "USD", testIBANDE, testIBANGB))
	if err != nil {
		t.Fatalf("ApplyPain007: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.RepairID != "" {
		t.Errorf("RepairID = %q, want empty on success", res.RepairID)
	}
	rev := res.Reversal
	if rev.ID != "pay-1/reversal" {
		t.Errorf("reversal ID = %q, want pay-1/reversal", rev.ID)
	}
	// The principal books debit=to / credit=from (transfer.go),
	// so the reversal swaps to debit=alice / credit=bob.
	if rev.DebitAccount != "alice" || rev.CreditAccount != "bob" {
		t.Errorf("reversal legs = %s/%s, want alice/bob (swapped from the principal)", rev.DebitAccount, rev.CreditAccount)
	}
	if rev.AmountCents != 15000 || rev.Currency != "USD" {
		t.Errorf("reversal = %d %s, want 15000 USD", rev.AmountCents, rev.Currency)
	}
	if rev.Memo != "reversal_of:pay-1" {
		t.Errorf("reversal memo = %q, want reversal_of:pay-1", rev.Memo)
	}
	if rev.Metadata["pain007_msg_id"] != "msg-1" || rev.Metadata["pain007_reason_code"] != "CUST" {
		t.Errorf("reversal metadata = %v, want pain007_msg_id/reason_code", rev.Metadata)
	}
	// The money is back: the transfer nets to zero.
	if got := l.BalanceIn("alice", "USD"); got != 0 {
		t.Errorf("alice balance = %d, want 0", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Errorf("bob balance = %d, want 0", got)
	}
	if l.Version() != before+1 {
		t.Errorf("version = %d, want %d", l.Version(), before+1)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	// Audit: one inbound marker with the exact details, plus the
	// reverse event ReverseEntry emits; no rejection.
	events := readAuditOps(t, al)
	if countOp(events, "pain007_reversal_applied") != 1 {
		t.Errorf("pain007_reversal_applied count = %d, want 1", countOp(events, "pain007_reversal_applied"))
	}
	if countOp(events, "pain007_reversal_rejected") != 0 {
		t.Errorf("pain007_reversal_rejected count = %d, want 0", countOp(events, "pain007_reversal_rejected"))
	}
	ev := findOp(events, "pain007_reversal_applied")
	if ev == nil {
		t.Fatal("pain007_reversal_applied event missing")
	}
	if ev.TraceID != "msg-1" {
		t.Errorf("trace = %q, want msg-1", ev.TraceID)
	}
	// Audit details decode from JSON, so numerics read as float64.
	if ev.Details["msg_id"] != "msg-1" || ev.Details["orgnl_end_to_end_id"] != "pay-1" ||
		ev.Details["reason_code"] != "CUST" || ev.Details["amount_cents"] != float64(15000) {
		t.Errorf("details = %v, want msg-1/pay-1/CUST/15000", ev.Details)
	}
}

func TestApplyPain007ReplaySameMsgID(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, testBICDE)
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	doc := pain007Doc("001.02", "msg-1", "pay-1", "CUST", "150.00", "USD", testIBANDE, testIBANGB)

	first, err := applyPain007Doc(t, l, doc)
	if err != nil {
		t.Fatalf("first ApplyPain007: %v", err)
	}
	versionAfterFirst := l.Version()
	// Replaying the same MsgId: the stored reversal comes back,
	// nothing new is booked, no second audit event fires.
	second, err := applyPain007Doc(t, l, doc)
	if err != nil {
		t.Fatalf("replay ApplyPain007: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if second.Reversal.ID != first.Reversal.ID {
		t.Errorf("replay reversal = %q, want %q", second.Reversal.ID, first.Reversal.ID)
	}
	if l.Version() != versionAfterFirst {
		t.Errorf("version moved on replay: %d -> %d", versionAfterFirst, l.Version())
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain007_reversal_applied") != 1 {
		t.Errorf("pain007_reversal_applied count = %d, want 1 (no second event on replay)", countOp(events, "pain007_reversal_applied"))
	}
}

func TestApplyPain007AlreadyReversedNoOp(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	if _, _, err := l.ReverseEntry("pay-1", "op-key"); err != nil {
		t.Fatalf("operator ReverseEntry: %v", err)
	}
	before := l.Version()
	// A cancellation request under a fresh MsgId for an already
	// reversed transfer is an idempotent no-op, not a conflict.
	res, err := applyPain007Doc(t, l, pain007Doc("001.02", "msg-2", "pay-1", "DUPL", "150.00", "USD", testIBANDE, testIBANGB))
	if err != nil {
		t.Fatalf("ApplyPain007: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on an already-reversed transfer, want true")
	}
	if res.Reversal.ID != "pay-1/reversal" {
		t.Errorf("reversal ID = %q, want pay-1/reversal", res.Reversal.ID)
	}
	if l.Version() != before {
		t.Errorf("version moved on no-op: %d -> %d", before, l.Version())
	}
}

func TestApplyPain007RejectedAudit(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, testBICDE)
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)

	// Unknown transfer and amount mismatch both park and emit the
	// rejection event.
	if _, err := applyPain007Doc(t, l, pain007Doc("001.02", "msg-u", "no-such", "CUST", "150.00", "USD", testIBANDE, testIBANGB)); !errors.Is(err, ErrPain007UnknownOriginal) {
		t.Fatalf("unknown: err = %v, want ErrPain007UnknownOriginal", err)
	}
	if _, err := applyPain007Doc(t, l, pain007Doc("001.02", "msg-m", "pay-1", "AM09", "150.01", "USD", testIBANDE, testIBANGB)); !errors.Is(err, ErrPain007AmountMismatch) {
		t.Fatalf("mismatch: err = %v, want ErrPain007AmountMismatch", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain007_reversal_rejected") != 2 {
		t.Errorf("pain007_reversal_rejected count = %d, want 2", countOp(events, "pain007_reversal_rejected"))
	}
	if countOp(events, "pain007_reversal_applied") != 0 {
		t.Errorf("pain007_reversal_applied count = %d, want 0", countOp(events, "pain007_reversal_applied"))
	}
	ev := findOp(events, "pain007_reversal_rejected")
	if ev == nil {
		t.Fatal("pain007_reversal_rejected event missing")
	}
	if ev.Details["msg_id"] != "msg-u" || ev.Details["orgnl_end_to_end_id"] != "no-such" {
		t.Errorf("rejection details = %v, want msg-u/no-such", ev.Details)
	}
}
