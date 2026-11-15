package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// pain013ApplyFixture builds a ledger with funded accounts and
// registered IBANs with BICs on both sides, but no RfP — the
// inbound pain.013 input.
func pain013ApplyFixture(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	return l
}

// pain013Doc renders one pain.013.001.07 document in the shape
// ExportPain013 renders: msgID asks for endToEndID, debtor IBAN
// pays creditor IBAN amount (decimal) in currency, expiring at
// xpry (RFC3339) and requested for execution on reqd (YYYY-MM-DD).
// Empty fields render as empty elements, so parse refusals can be
// driven per field. version selects the namespace (001.07 valid,
// anything else the wrong schema version).
func pain013Doc(version, msgID, endToEndID, dbtrIBAN, cdtrIBAN, amount, currency, reqd, xpry string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.013.` + version + `">` + "\n")
	sb.WriteString("<CdtrPmtActvtnReq>\n<GrpHdr>\n")
	sb.WriteString("<MsgId>" + msgID + "</MsgId>\n")
	sb.WriteString("<CreDtTm>2026-11-14T10:00:00Z</CreDtTm>\n")
	sb.WriteString("<NbOfTxs>1</NbOfTxs>\n<InitgPty><Nm>LEDGER</Nm></InitgPty>\n</GrpHdr>\n<PmtInf>\n")
	sb.WriteString("<PmtInfId>" + msgID + "</PmtInfId>\n<PmtMtd>TRF</PmtMtd>\n")
	sb.WriteString("<ReqdExctnDt><Dt>" + reqd + "</Dt></ReqdExctnDt>\n")
	sb.WriteString("<XpryDt><DtTm>" + xpry + "</DtTm></XpryDt>\n")
	sb.WriteString("<Dbtr><Nm>customer</Nm></Dbtr>\n")
	sb.WriteString("<DbtrAcct><Id><IBAN>" + dbtrIBAN + "</IBAN></Id></DbtrAcct>\n")
	sb.WriteString("<DbtrAgt><FinInstnId><BIC>DEUTDEFF</BIC></FinInstnId></DbtrAgt>\n")
	sb.WriteString("<CdtTrfTx>\n")
	sb.WriteString("<PmtId><EndToEndId>" + endToEndID + "</EndToEndId></PmtId>\n")
	sb.WriteString(`<Amt><InstdAmt Ccy="` + currency + `">` + amount + `</InstdAmt></Amt>` + "\n")
	sb.WriteString("<ChrgBr>SLEV</ChrgBr>\n")
	sb.WriteString("<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>\n")
	sb.WriteString("<Cdtr><Nm>merchant</Nm></Cdtr>\n")
	sb.WriteString("<CdtrAcct><Id><IBAN>" + cdtrIBAN + "</IBAN></Id></CdtrAcct>\n")
	sb.WriteString("</CdtTrfTx>\n</PmtInf>\n</CdtrPmtActvtnReq>\n</Document>\n")
	return sb.String()
}

func pain013FutureExpiry() string {
	return time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
}

// pain013ValidDoc is the happy-path document: msg-013-1 asks for
// rfp-in-013, customer pays merchant USD 50.00, expiring in an
// hour, requested for execution today.
func pain013ValidDoc() string {
	return pain013Doc("001.07", "msg-013-1", "rfp-in-013",
		testIBANDE, testIBANGB, "50.00", "USD",
		time.Now().UTC().Format("2006-01-02"), pain013FutureExpiry())
}

func TestParsePain013_RoundTrip(t *testing.T) {
	l, r := pain013Fixture(t)

	doc, err := l.ExportPain013(r.ID)
	if err != nil {
		t.Fatalf("ExportPain013: %v", err)
	}
	req, err := ParsePain013(doc)
	if err != nil {
		t.Fatalf("ParsePain013(export): %v", err)
	}
	if req.MsgID != "pain013-rfp-013" {
		t.Errorf("MsgID = %q, want pain013-rfp-013", req.MsgID)
	}
	if req.PmtInfID != "pain013-rfp-013" {
		t.Errorf("PmtInfID = %q, want pain013-rfp-013", req.PmtInfID)
	}
	if req.EndToEndID != r.ID {
		t.Errorf("EndToEndID = %q, want %q", req.EndToEndID, r.ID)
	}
	if req.DebtorIBAN != testIBANDE || req.CreditorIBAN != testIBANGB {
		t.Errorf("IBANs = %q/%q, want %q/%q", req.DebtorIBAN, req.CreditorIBAN, testIBANDE, testIBANGB)
	}
	if req.DebtorName != "customer" || req.CreditorName != "merchant" {
		t.Errorf("names = %q/%q, want customer/merchant", req.DebtorName, req.CreditorName)
	}
	if req.DebtorBIC != "DEUTDEFF" || req.CreditorBIC != "NWBKGB2L" {
		t.Errorf("BICs = %q/%q, want DEUTDEFF/NWBKGB2L", req.DebtorBIC, req.CreditorBIC)
	}
	if req.AmountCents != 5000 || req.Currency != "USD" {
		t.Errorf("amount = %d %q, want 5000 USD", req.AmountCents, req.Currency)
	}
	// The export renders ReqdExctnDt as the RfP's creation date
	// (<Dt>) and XpryDt as the full ExpiresAt (<DtTm>, second
	// precision — the RFC3339 rendering drops sub-seconds).
	if !req.ReqdExctnDt.Equal(r.CreatedAt.Truncate(24 * time.Hour)) {
		t.Errorf("ReqdExctnDt = %v, want the RfP creation date %v", req.ReqdExctnDt, r.CreatedAt)
	}
	if req.ExpiresAt.Unix() != r.ExpiresAt.Unix() {
		t.Errorf("ExpiresAt = %v, want %v", req.ExpiresAt, r.ExpiresAt)
	}
}

func TestParsePain013_WrongNamespace(t *testing.T) {
	// pain.013.001.05 is a real registry version, but the ledger
	// only speaks .001.07: the namespace gate refuses it.
	bad := pain013Doc("001.05", "msg-013-ns", "rfp-in-013",
		testIBANDE, testIBANGB, "50.00", "USD",
		time.Now().UTC().Format("2006-01-02"), pain013FutureExpiry())
	if _, err := ParsePain013([]byte(bad)); !errors.Is(err, ErrInvalidPain013) {
		t.Errorf("wrong namespace: err = %v, want ErrInvalidPain013", err)
	}
	if _, err := ParsePain013([]byte("not xml at all")); !errors.Is(err, ErrInvalidPain013) {
		t.Errorf("garbage bytes: err = %v, want ErrInvalidPain013", err)
	}
	if _, err := ParsePain013([]byte("")); !errors.Is(err, ErrInvalidPain013) {
		t.Errorf("empty document: err = %v, want ErrInvalidPain013", err)
	}
}

func TestParsePain013_MissingFields(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02")
	future := pain013FutureExpiry()
	cases := []struct {
		name string
		doc  string
	}{
		{"missing MsgId", pain013Doc("001.07", "", "rfp-in-013", testIBANDE, testIBANGB, "50.00", "USD", now, future)},
		{"missing EndToEndId", pain013Doc("001.07", "msg-013-1", "", testIBANDE, testIBANGB, "50.00", "USD", now, future)},
		{"missing debtor IBAN", pain013Doc("001.07", "msg-013-1", "rfp-in-013", "", testIBANGB, "50.00", "USD", now, future)},
		{"missing creditor IBAN", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, "", "50.00", "USD", now, future)},
		{"missing ReqdExctnDt", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "50.00", "USD", "", future)},
		{"missing XpryDt", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "50.00", "USD", now, "")},
		{"missing InstdAmt", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "", "USD", now, future)},
		{"missing currency", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "50.00", "", now, future)},
		{"malformed amount", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "12.345", "USD", now, future)},
		{"non-numeric amount", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "fifty", "USD", now, future)},
		{"bad currency", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "50.00", "US", now, future)},
		{"malformed expiry", pain013Doc("001.07", "msg-013-1", "rfp-in-013", testIBANDE, testIBANGB, "50.00", "USD", now, "not-a-date")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePain013([]byte(tc.doc)); !errors.Is(err, ErrInvalidPain013) {
				t.Errorf("err = %v, want ErrInvalidPain013", err)
			}
		})
	}
	// A non-positive amount parses (it is a CreateRfP refusal,
	// not a parse refusal) and an XpryDt <Dt> date parses too.
	for _, amt := range []string{"0.00", "-5.00"} {
		req, err := ParsePain013([]byte(pain013Doc("001.07", "msg-013-1", "rfp-in-013",
			testIBANDE, testIBANGB, amt, "USD", now, future)))
		if err != nil {
			t.Errorf("amount %q: err = %v, want a parsed request", amt, err)
		} else if req.AmountCents >= 0 && amt == "-5.00" {
			t.Errorf("amount %q: parsed %d cents, want negative", amt, req.AmountCents)
		}
	}
}

func TestApplyPain013_RegistersRfP(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")

	before := l.Version()
	req, err := ParsePain013([]byte(pain013ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain013: %v", err)
	}
	res, err := l.ApplyPain013(req, "ops")
	if err != nil {
		t.Fatalf("ApplyPain013: %v", err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	if res.RepairID != "" {
		t.Errorf("RepairID = %q on a successful apply, want empty", res.RepairID)
	}
	got, ok := l.GetRfP("rfp-in-013")
	if !ok {
		t.Fatal("GetRfP: RfP not found after registration")
	}
	if got.Status != RfPStatusIssued {
		t.Errorf("status = %q, want issued (registration is not acceptance)", got.Status)
	}
	if got.Creditor != "merchant" || got.Debtor != "customer" {
		t.Errorf("parties = %q/%q, want merchant/customer", got.Creditor, got.Debtor)
	}
	if got.AmountCents != 5000 || got.Currency != "USD" {
		t.Errorf("amount = %d %q, want 5000 USD", got.AmountCents, got.Currency)
	}
	if got.ExpiresAt.Unix() != req.ExpiresAt.Unix() {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, req.ExpiresAt)
	}
	// Registration is structural state, like CreateRfP: no
	// version bump, no journal rows.
	if l.Version() != before {
		t.Errorf("version moved %d -> %d on a registration", before, l.Version())
	}
	// The inbound direction marker, distinct from rfp_issued.
	events := readAuditOps(t, al)
	ev := findOp(events, "pain013_request_received")
	if ev == nil {
		t.Fatal("pain013_request_received audit event missing")
	}
	if countOp(events, "pain013_request_received") != 1 {
		t.Errorf("pain013_request_received count = %d, want 1", countOp(events, "pain013_request_received"))
	}
	if countOp(events, "rfp_issued") != 1 {
		t.Errorf("rfp_issued count = %d, want 1", countOp(events, "rfp_issued"))
	}
	if ev.TraceID != "msg-013-1" {
		t.Errorf("trace_id = %q, want the GrpHdr MsgId", ev.TraceID)
	}
	for k, want := range map[string]any{
		"msg_id": "msg-013-1", "rfp_id": "rfp-in-013",
		"creditor": "merchant", "debtor": "customer",
		"amount_cents": float64(5000), "currency": "USD",
		"expires_at": req.ExpiresAt.UTC().Format(time.RFC3339),
	} {
		if ev.Details[k] != want {
			t.Errorf("audit detail %s = %v, want %v", k, ev.Details[k], want)
		}
	}
}

func TestApplyPain013_UnknownAccount(t *testing.T) {
	l := pain013ApplyFixture(t)

	// The debtor IBAN names no registered account: fail-closed
	// into the repair queue, never auto-opened.
	unknownDebtor := pain013Doc("001.07", "msg-013-unk", "rfp-in-unk",
		"FR1420041010050500013M02606", testIBANGB, "50.00", "USD",
		time.Now().UTC().Format("2006-01-02"), pain013FutureExpiry())
	req, err := ParsePain013([]byte(unknownDebtor))
	if err != nil {
		t.Fatalf("ParsePain013: %v", err)
	}
	res, err := l.ApplyPain013(req, "ops")
	if !errors.Is(err, ErrPain013UnknownAccount) {
		t.Fatalf("err = %v, want ErrPain013UnknownAccount", err)
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty: the unknown account was not parked")
	}
	if rec, ok := l.GetRepair(res.RepairID); !ok {
		t.Errorf("repair record %q not found", res.RepairID)
	} else if rec.ReasonCode != RepairReasonUnknownRoute {
		t.Errorf("repair reason = %q, want unknown_route", rec.ReasonCode)
	}
	if _, ok := l.GetRfP("rfp-in-unk"); ok {
		t.Error("RfP registered for an unknown account: the fail-closed gate leaked")
	}

	// The creditor side is gated the same way.
	unknownCreditor := pain013Doc("001.07", "msg-013-unk2", "rfp-in-unk2",
		testIBANDE, "FR1420041010050500013M02606", "50.00", "USD",
		time.Now().UTC().Format("2006-01-02"), pain013FutureExpiry())
	req, err = ParsePain013([]byte(unknownCreditor))
	if err != nil {
		t.Fatalf("ParsePain013: %v", err)
	}
	res, err = l.ApplyPain013(req, "ops")
	if !errors.Is(err, ErrPain013UnknownAccount) {
		t.Fatalf("creditor: err = %v, want ErrPain013UnknownAccount", err)
	}
	if res.RepairID == "" {
		t.Error("creditor: RepairID empty")
	}
	if _, ok := l.GetRfP("rfp-in-unk2"); ok {
		t.Error("RfP registered for an unknown creditor")
	}
}

func TestApplyPain013_InvalidTermsParked(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02")
	future := pain013FutureExpiry()
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name    string
		doc     string
		wantErr error
	}{
		{"same party", pain013Doc("001.07", "msg-013-same", "rfp-in-same",
			testIBANDE, testIBANDE, "50.00", "USD", now, future), ErrRfPSameParty},
		{"zero amount", pain013Doc("001.07", "msg-013-zero", "rfp-in-zero",
			testIBANDE, testIBANGB, "0.00", "USD", now, future), ErrRfPNonPositiveAmount},
		{"negative amount", pain013Doc("001.07", "msg-013-neg", "rfp-in-neg",
			testIBANDE, testIBANGB, "-5.00", "USD", now, future), ErrRfPNonPositiveAmount},
		{"expiry not in future", pain013Doc("001.07", "msg-013-past", "rfp-in-past",
			testIBANDE, testIBANGB, "50.00", "USD", now, past), ErrRfPExpiryNotFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := pain013ApplyFixture(t)
			req, err := ParsePain013([]byte(tc.doc))
			if err != nil {
				t.Fatalf("ParsePain013: %v", err)
			}
			res, err := l.ApplyPain013(req, "ops")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if res.RepairID == "" {
				t.Fatal("RepairID empty: the invalid request was not parked")
			}
			if rec, ok := l.GetRepair(res.RepairID); !ok {
				t.Errorf("repair record %q not found", res.RepairID)
			} else if rec.ReasonCode != RepairReasonInvalidFormat {
				t.Errorf("repair reason = %q, want invalid_format", rec.ReasonCode)
			}
			if _, ok := l.GetRfP(req.EndToEndID); ok {
				t.Error("RfP registered for an invalid request")
			}
		})
	}
}

func TestApplyPain013_ReplayIdempotent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")

	req, err := ParsePain013([]byte(pain013ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain013: %v", err)
	}
	first, err := l.ApplyPain013(req, "ops")
	if err != nil {
		t.Fatalf("first ApplyPain013: %v", err)
	}
	second, err := l.ApplyPain013(req, "ops")
	if err != nil {
		t.Fatalf("replay ApplyPain013: %v", err)
	}
	if !second.Duplicate {
		t.Error("Duplicate = false on replay, want true")
	}
	if second.RfP.ID != first.RfP.ID || second.RfP.Status != RfPStatusIssued {
		t.Errorf("replay returned %+v, want the stored issued RfP", second.RfP)
	}
	// No second registration, no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "rfp_issued") != 1 {
		t.Errorf("rfp_issued count = %d, want 1", countOp(events, "rfp_issued"))
	}
	if countOp(events, "pain013_request_received") != 1 {
		t.Errorf("pain013_request_received count = %d, want 1", countOp(events, "pain013_request_received"))
	}
}

func TestApplyPain013_MsgIDConflict(t *testing.T) {
	l := pain013ApplyFixture(t)

	req, err := ParsePain013([]byte(pain013ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain013: %v", err)
	}
	if _, err := l.ApplyPain013(req, "ops"); err != nil {
		t.Fatalf("first ApplyPain013: %v", err)
	}
	// The same MsgId for a different amount: refused, never
	// silently overwritten, and not parked (a protocol
	// violation, not a repairable request).
	conflict := pain013Doc("001.07", "msg-013-1", "rfp-in-013",
		testIBANDE, testIBANGB, "60.00", "USD",
		time.Now().UTC().Format("2006-01-02"), pain013FutureExpiry())
	creq, err := ParsePain013([]byte(conflict))
	if err != nil {
		t.Fatalf("ParsePain013: %v", err)
	}
	res, err := l.ApplyPain013(creq, "ops")
	if !errors.Is(err, ErrPain013MsgIDConflict) {
		t.Fatalf("err = %v, want ErrPain013MsgIDConflict", err)
	}
	if res.RepairID != "" {
		t.Errorf("RepairID = %q on a conflict, want empty", res.RepairID)
	}
	got, ok := l.GetRfP("rfp-in-013")
	if !ok {
		t.Fatal("GetRfP: original RfP vanished")
	}
	if got.AmountCents != 5000 {
		t.Errorf("amount = %d, want the original 5000 (no overwrite)", got.AmountCents)
	}
}

func TestApplyPain013_AcceptThenPay(t *testing.T) {
	ctx := context.Background()
	l := pain013ApplyFixture(t)

	req, err := ParsePain013([]byte(pain013ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain013: %v", err)
	}
	if _, err := l.ApplyPain013(req, "ops"); err != nil {
		t.Fatalf("ApplyPain013: %v", err)
	}
	// Acceptance still goes through AcceptRfP, unchanged: the
	// payout posts debtor -> creditor through the full gate
	// sequence, and the RfP ends paid.
	receipt, err := l.AcceptRfP(ctx, "rfp-in-013", "customer")
	if err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	if receipt.TransferID != "rfp/rfp-in-013/payout" {
		t.Errorf("transfer = %q, want rfp/rfp-in-013/payout", receipt.TransferID)
	}
	got, ok := l.GetRfP("rfp-in-013")
	if !ok {
		t.Fatal("GetRfP: RfP vanished")
	}
	if got.Status != RfPStatusPaid {
		t.Errorf("status = %q, want paid", got.Status)
	}
	if got.TransferID != receipt.TransferID {
		t.Errorf("TransferID = %q, want %q", got.TransferID, receipt.TransferID)
	}
}
