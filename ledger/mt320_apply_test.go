package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt320ExportedDoc renders the fixture deposit's MT320 through the
// outbound export, the counterparty's-eye view of the same contract.
func mt320ExportedDoc(t *testing.T, l *Ledger, td TermDeposit) string {
	t.Helper()
	doc, err := l.ExportMT320(MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: "NWBKGB2L"})
	if err != nil {
		t.Fatalf("ExportMT320: %v", err)
	}
	return doc
}

// mt320ParsedConfirmation parses doc, failing the test on any
// parse error.
func mt320ParsedConfirmation(t *testing.T, doc string) MT320Confirmation {
	t.Helper()
	req, err := ParseMT320(doc)
	if err != nil {
		t.Fatalf("ParseMT320: %v", err)
	}
	return req
}

func TestParseMT320_RoundTrip(t *testing.T) {
	l, td := mt320TestLedger(t)
	req := mt320ParsedConfirmation(t, mt320ExportedDoc(t, l, td))

	if req.SenderLT != "DEUTDEFFXXX" {
		t.Errorf("SenderLT = %q, want DEUTDEFFXXX", req.SenderLT)
	}
	if req.ReceiverLT != "NWBKGB2LXXX" {
		t.Errorf("ReceiverLT = %q, want NWBKGB2LXXX", req.ReceiverLT)
	}
	if req.Reference != td.ID {
		t.Errorf("Reference = %q, want %q", req.Reference, td.ID)
	}
	if req.Variant != "NEWT" {
		t.Errorf("Variant = %q, want NEWT", req.Variant)
	}
	if req.ConfirmationType != "CONF" {
		t.Errorf("ConfirmationType = %q, want CONF", req.ConfirmationType)
	}
	if req.CommonReference != "DEUTNW1001NWBKDE" {
		t.Errorf("CommonReference = %q, want DEUTNW1001NWBKDE", req.CommonReference)
	}
	if req.Depositor != "DEUTDEFF" || !req.DepositorBIC {
		t.Errorf("Depositor = %q (BIC=%v), want DEUTDEFF (BIC=true)", req.Depositor, req.DepositorBIC)
	}
	if req.Counterparty != "NWBKGB2L" || !req.CounterpartyBIC {
		t.Errorf("Counterparty = %q (BIC=%v), want NWBKGB2L (BIC=true)", req.Counterparty, req.CounterpartyBIC)
	}
	if req.Role != "L" {
		t.Errorf("Role = %q, want L", req.Role)
	}
	opened := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	matures := time.Date(2027, 1, 30, 0, 0, 0, 0, time.UTC)
	if !req.TradeDate.Equal(opened) {
		t.Errorf("TradeDate = %v, want %v", req.TradeDate, opened)
	}
	if !req.ValueDate.Equal(opened) {
		t.Errorf("ValueDate = %v, want %v", req.ValueDate, opened)
	}
	if !req.MaturityDate.Equal(matures) {
		t.Errorf("MaturityDate = %v, want %v", req.MaturityDate, matures)
	}
	if req.PrincipalCurrency != "USD" || req.PrincipalCents != 400_000 {
		t.Errorf("principal = %s %d, want USD 400000", req.PrincipalCurrency, req.PrincipalCents)
	}
	if req.InterestCurrency != "USD" || req.InterestCents != 4860 {
		t.Errorf("interest = %s %d, want USD 4860", req.InterestCurrency, req.InterestCents)
	}
	if req.Rate != "0,05" {
		t.Errorf("Rate = %q, want 0,05", req.Rate)
	}
	if req.DayCount != "ACT/365" {
		t.Errorf("DayCount = %q, want ACT/365", req.DayCount)
	}
	if req.ReceivingBank != "NWBKGB2L" || !req.ReceivingBankBIC {
		t.Errorf("ReceivingBank = %q (BIC=%v), want NWBKGB2L (BIC=true)", req.ReceivingBank, req.ReceivingBankBIC)
	}
	if req.PayingBank != "DEUTDEFF" || !req.PayingBankBIC {
		t.Errorf("PayingBank = %q (BIC=%v), want DEUTDEFF (BIC=true)", req.PayingBank, req.PayingBankBIC)
	}
}

func TestParseMT320_RejectsBadBlockStructure(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt320ExportedDoc(t, l, td)

	// Wrong message type in block 2.
	if _, err := ParseMT320(strings.Replace(doc, "{2:I320", "{2:I321", 1)); err == nil {
		t.Error("block 2 I321: want an error, got nil")
	}
	// Missing trailer.
	if _, err := ParseMT320(strings.TrimSuffix(doc, "-}")); err == nil {
		t.Error("missing -} trailer: want an error, got nil")
	}
	// CRLF line endings are accepted, and a trailing newline after
	// the trailer is tolerated.
	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT320(crlf); err != nil {
		t.Errorf("CRLF endings: want nil, got %v", err)
	}
	if _, err := ParseMT320(doc + "\n"); err != nil {
		t.Errorf("trailing newline: want nil, got %v", err)
	}
}

func TestParseMT320_RejectsUnknownField(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt320ExportedDoc(t, l, td)

	// A field outside the twenty-field core set, at the same line
	// count so the framing check cannot catch it first.
	unknown := strings.Replace(doc, ":14D:ACT/365", ":77D:NOTE", 1)
	if _, err := ParseMT320(unknown); err == nil {
		t.Error("unknown :77D: field: want an error, got nil")
	}
	// An extra field line breaks the fixed line count.
	extra := strings.Replace(doc, ":14D:ACT/365\n", ":14D:ACT/365\n:77D:NOTE\n", 1)
	if _, err := ParseMT320(extra); err == nil {
		t.Error("extra field line: want an error, got nil")
	}
}

func TestParseMT320_RejectsNonNewtVariant(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt320ExportedDoc(t, l, td)

	amnd := strings.Replace(doc, ":22A:NEWT", ":22A:AMND", 1)
	if _, err := ParseMT320(amnd); err == nil {
		t.Error(":22A:AMND: want an error, got nil")
	}
}

func TestParseMT320_RejectsNonConfVariant(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt320ExportedDoc(t, l, td)

	prea := strings.Replace(doc, ":22B:CONF", ":22B:PREA", 1)
	if _, err := ParseMT320(prea); err == nil {
		t.Error(":22B:PREA: want an error, got nil")
	}
}

func TestApplyMT320_ConfirmsMatchedDeposit(t *testing.T) {
	l, td := mt320TestLedger(t)
	req := mt320ParsedConfirmation(t, mt320ExportedDoc(t, l, td))

	res, err := l.ApplyMT320(req, "ops")
	if err != nil {
		t.Fatalf("ApplyMT320: %v", err)
	}
	if res.Deposit == nil || res.Deposit.ID != td.ID {
		t.Fatalf("Deposit = %+v, want ID %q", res.Deposit, td.ID)
	}
	if res.CommonReference != "DEUTNW1001NWBKDE" {
		t.Errorf("CommonReference = %q, want DEUTNW1001NWBKDE", res.CommonReference)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	// The confirmation is read-only: the contract stays active.
	if res.Deposit.Status != TermDepositActive {
		t.Errorf("Deposit status = %q, want active (confirmation moves no lifecycle state)", res.Deposit.Status)
	}
	ref, ok := l.DepositConfirmed(td.ID)
	if !ok || ref != td.ID {
		t.Errorf("DepositConfirmed = (%q, %v), want (%q, true)", ref, ok, td.ID)
	}
}

func TestApplyMT320_ConfirmsMaturedDeposit(t *testing.T) {
	l, td := mt320TestLedger(t)
	if _, err := l.MatureTermDeposit(td.ID, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	req := mt320ParsedConfirmation(t, mt320ExportedDoc(t, l, td))

	res, err := l.ApplyMT320(req, "ops")
	if err != nil {
		t.Fatalf("ApplyMT320 on matured deposit: %v", err)
	}
	if res.Deposit.Status != TermDepositMatured {
		t.Errorf("Deposit status = %q, want matured", res.Deposit.Status)
	}
}

func TestApplyMT320_RefusesUnknownDeposit(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.ReplaceAll(mt320ExportedDoc(t, l, td), td.ID, "td-mt320-9999")
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("ApplyMT320: err = %v, want ErrTermDepositNotFound", err)
	}
}

func TestApplyMT320_RefusesRateMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.Replace(mt320ExportedDoc(t, l, td), ":37G:0,05", ":37G:0,06", 1)
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrMT320TermsMismatch) {
		t.Errorf("ApplyMT320: err = %v, want ErrMT320TermsMismatch", err)
	}
}

func TestApplyMT320_RefusesPrincipalMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.Replace(mt320ExportedDoc(t, l, td), ":32B:USD4000,00", ":32B:USD4001,00", 1)
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrMT320TermsMismatch) {
		t.Errorf("ApplyMT320: err = %v, want ErrMT320TermsMismatch", err)
	}
}

func TestApplyMT320_RefusesInterestMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.Replace(mt320ExportedDoc(t, l, td), ":34E:USD48,60", ":34E:USD48,61", 1)
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrMT320TermsMismatch) {
		t.Errorf("ApplyMT320: err = %v, want ErrMT320TermsMismatch", err)
	}
}

func TestApplyMT320_RefusesMaturityMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.Replace(mt320ExportedDoc(t, l, td), ":30P:20270130", ":30P:20270131", 1)
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrMT320TermsMismatch) {
		t.Errorf("ApplyMT320: err = %v, want ErrMT320TermsMismatch", err)
	}
}

func TestApplyMT320_RefusesCommonReferenceMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.Replace(mt320ExportedDoc(t, l, td), ":22C:DEUTNW1001NWBKDE", ":22C:DEUTNW1001NWBKDF", 1)
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrMT320TermsMismatch) {
		t.Errorf("ApplyMT320: err = %v, want ErrMT320TermsMismatch", err)
	}
}

func TestApplyMT320_RefusesSettlementLegMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	// Sequence C must name the counterparty, not the depositor.
	doc := strings.Replace(mt320ExportedDoc(t, l, td), ":57A:NWBKGB2L", ":57A:DEUTDEFF", 1)
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrMT320TermsMismatch) {
		t.Errorf("ApplyMT320: err = %v, want ErrMT320TermsMismatch", err)
	}
}

func TestApplyMT320_ReplaySameReferenceNoop(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt320ExportedDoc(t, l, td)
	req := mt320ParsedConfirmation(t, doc)

	if _, err := l.ApplyMT320(req, "ops"); err != nil {
		t.Fatalf("first ApplyMT320: %v", err)
	}
	res, err := l.ApplyMT320(req, "ops")
	if err != nil {
		t.Fatalf("replay ApplyMT320: %v", err)
	}
	if !res.Duplicate {
		t.Error("Duplicate = false on replay, want true")
	}
	if res.Deposit == nil || res.Deposit.ID != td.ID {
		t.Fatalf("replay Deposit = %+v, want ID %q", res.Deposit, td.ID)
	}
	// The same :20: with different terms is a conflict, never a
	// silent overwrite.
	changed := strings.Replace(doc, ":37G:0,05", ":37G:0,06", 1)
	changedReq := mt320ParsedConfirmation(t, changed)
	if _, err := l.ApplyMT320(changedReq, "ops"); !errors.Is(err, ErrMT320MsgIDConflict) {
		t.Errorf("ApplyMT320 with changed terms: err = %v, want ErrMT320MsgIDConflict", err)
	}
}

func TestApplyMT320_RefusesWithdrawnDeposit(t *testing.T) {
	l, td := mt320TestLedger(t)
	// Export first: a withdrawn deposit cannot be exported, so
	// the counterparty's confirmation must predate the withdrawal.
	req := mt320ParsedConfirmation(t, mt320ExportedDoc(t, l, td))
	if _, err := l.WithdrawTermDeposit(td.ID, 0, time.Now()); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}

	if _, err := l.ApplyMT320(req, "ops"); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("ApplyMT320: err = %v, want ErrTermDepositNotActive", err)
	}
}
