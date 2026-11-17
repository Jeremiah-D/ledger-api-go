package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt321ExportedDoc renders the fixture deposit's MT321 through the
// outbound export, the fund manager's-eye view of the same contract.
func mt321ExportedDoc(t *testing.T, l *Ledger, td TermDeposit) string {
	t.Helper()
	doc, err := l.ExportMT321(MT321ExportOptions{DepositID: td.ID, CustodianBIC: "CHASUS33", CounterpartyBIC: "NWBKGB2L"})
	if err != nil {
		t.Fatalf("ExportMT321: %v", err)
	}
	return doc
}

// mt321ParsedInstruction parses doc, failing the test on any
// parse error.
func mt321ParsedInstruction(t *testing.T, doc string) MT321Instruction {
	t.Helper()
	req, err := ParseMT321(doc)
	if err != nil {
		t.Fatalf("ParseMT321: %v", err)
	}
	return req
}

// mt321AuditLedger builds the mt320TestLedger fixture on a ledger
// wired to an audit log, so apply tests can assert the audit
// trail.
func mt321AuditLedger(t *testing.T, al *AuditLog) (*Ledger, TermDeposit) {
	t.Helper()
	l := New(WithTermDepositInterestExpense("td-interest-expense"), WithAuditLog(al))
	seedBalance(t, l, "saver", 1_000_000)
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN saver: %v", err)
	}
	opened := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-mt320-1001",
		Account:     "saver",
		AmountCents: 400_000,
		Currency:    "USD",
		TermDays:    90,
		RateBps:     500,
		OpenedAt:    opened,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	return l, td
}

func TestParseMT321_RoundTrip(t *testing.T) {
	l, td := mt320TestLedger(t)
	req := mt321ParsedInstruction(t, mt321ExportedDoc(t, l, td))

	if req.SenderLT != "DEUTDEFFXXX" {
		t.Errorf("SenderLT = %q, want DEUTDEFFXXX", req.SenderLT)
	}
	if req.ReceiverLT != "CHASUS33XXX" {
		t.Errorf("ReceiverLT = %q, want CHASUS33XXX", req.ReceiverLT)
	}
	if req.Reference != td.ID {
		t.Errorf("Reference = %q, want %q", req.Reference, td.ID)
	}
	if req.CommonReference != "DEUTNW1001NWBKDE" {
		t.Errorf("CommonReference = %q, want DEUTNW1001NWBKDE", req.CommonReference)
	}
	if req.Variant != "NEWM" {
		t.Errorf("Variant = %q, want NEWM", req.Variant)
	}
	if req.TLDE != "CONF" {
		t.Errorf("TLDE = %q, want CONF", req.TLDE)
	}
	if req.ContractNumber != td.ID {
		t.Errorf("ContractNumber = %q, want %q", req.ContractNumber, td.ID)
	}
	if req.DepositOrLoan != "LEND" {
		t.Errorf("DepositOrLoan = %q, want LEND", req.DepositOrLoan)
	}
	if req.DayCount != "A009" {
		t.Errorf("DayCount = %q, want A009", req.DayCount)
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
	if req.SettlementCurrency != "USD" || req.SettlementCents != 400_000 {
		t.Errorf("settlement = %s %d, want USD 400000", req.SettlementCurrency, req.SettlementCents)
	}
	if req.Rate != "0,05" {
		t.Errorf("Rate = %q, want 0,05", req.Rate)
	}
	if req.Counterparty != "NWBKGB2L" || !req.CounterpartyBIC {
		t.Errorf("Counterparty = %q (BIC=%v), want NWBKGB2L (BIC=true)", req.Counterparty, req.CounterpartyBIC)
	}
	if req.Investor != "DEUTDEFF" || !req.InvestorBIC {
		t.Errorf("Investor = %q (BIC=%v), want DEUTDEFF (BIC=true)", req.Investor, req.InvestorBIC)
	}
	if req.SafekeepingAccount != "DE89370400440532013000" {
		t.Errorf("SafekeepingAccount = %q, want DE89370400440532013000", req.SafekeepingAccount)
	}
	if req.SettlementPriority != "APFM" {
		t.Errorf("SettlementPriority = %q, want APFM", req.SettlementPriority)
	}
	if req.AccountWith != "NWBKGB2L" || !req.AccountWithBIC {
		t.Errorf("AccountWith = %q (BIC=%v), want NWBKGB2L (BIC=true)", req.AccountWith, req.AccountWithBIC)
	}
	if req.CashDeliveryAgent != "DEUTDEFF" || !req.CashDeliveryAgentBIC {
		t.Errorf("CashDeliveryAgent = %q (BIC=%v), want DEUTDEFF (BIC=true)", req.CashDeliveryAgent, req.CashDeliveryAgentBIC)
	}
}

func TestParseMT321_RejectsBadBlockStructure(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt321ExportedDoc(t, l, td)

	// Wrong message type in block 2.
	if _, err := ParseMT321(strings.Replace(doc, "{2:I321", "{2:I320", 1)); err == nil {
		t.Error("block 2 I320: want an error, got nil")
	}
	// Missing trailer.
	if _, err := ParseMT321(strings.TrimSuffix(doc, "-}")); err == nil {
		t.Error("missing -} trailer: want an error, got nil")
	}
	// CRLF line endings are accepted, and a trailing newline after
	// the trailer is tolerated.
	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT321(crlf); err != nil {
		t.Errorf("CRLF endings: want nil, got %v", err)
	}
	if _, err := ParseMT321(doc + "\n"); err != nil {
		t.Errorf("trailing newline: want nil, got %v", err)
	}
}

func TestParseMT321_RejectsNonNewmVariant(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt321ExportedDoc(t, l, td)

	canc := strings.Replace(doc, ":23G:NEWM", ":23G:CANC", 1)
	if _, err := ParseMT321(canc); err == nil {
		t.Error(":23G:CANC: want an error, got nil")
	}
}

func TestParseMT321_RejectsNonConfTLDE(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt321ExportedDoc(t, l, td)

	matu := strings.Replace(doc, ":22H::TLDE//CONF", ":22H::TLDE//MATU", 1)
	if _, err := ParseMT321(matu); err == nil {
		t.Error(":22H::TLDE//MATU: want an error, got nil")
	}
}

func TestParseMT321_RejectsMissingField(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := mt321ExportedDoc(t, l, td)

	// The :19A::SETT// line replaced by a duplicate :19A::PRIN//
	// line: same line count, so the framing check cannot catch it
	// first — the field check must.
	missing := strings.Replace(doc, ":19A::SETT//USD4000,00", ":19A::PRIN//USD4000,00", 1)
	if _, err := ParseMT321(missing); err == nil {
		t.Error("missing :19A::SETT//: want an error, got nil")
	}
	// An extra field line breaks the fixed line count.
	extra := strings.Replace(doc, ":22H::MICO//A009\n", ":22H::MICO//A009\n:70C::PACO\n", 1)
	if _, err := ParseMT321(extra); err == nil {
		t.Error("extra field line: want an error, got nil")
	}
}

func TestApplyMT321_RegistersInstruction(t *testing.T) {
	al := mustAuditLog(t)
	l, td := mt321AuditLedger(t, al)
	req := mt321ParsedInstruction(t, mt321ExportedDoc(t, l, td))

	res, err := l.ApplyMT321(req, "ops")
	if err != nil {
		t.Fatalf("ApplyMT321: %v", err)
	}
	if res.Instruction == nil || res.Instruction.Reference != td.ID {
		t.Fatalf("Instruction = %+v, want reference %q", res.Instruction, td.ID)
	}
	if res.DepositID != td.ID {
		t.Errorf("DepositID = %q, want %q", res.DepositID, td.ID)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	if res.Instruction.CommonReference != "DEUTNW1001NWBKDE" {
		t.Errorf("CommonReference = %q, want DEUTNW1001NWBKDE", res.Instruction.CommonReference)
	}
	// Registration moves no cash and no lifecycle state: the
	// contract stays active.
	if td2, ok := l.GetTermDeposit(td.ID); !ok || td2.Status != TermDepositActive {
		t.Errorf("Deposit status = %q, want active (registration moves no lifecycle state)", td2.Status)
	}
	// The registry holds the instruction under the SEME.
	stored, ok := l.GetMT321Instruction(td.ID)
	if !ok {
		t.Fatal("GetMT321Instruction: want the registered instruction, got none")
	}
	if stored.CommonReference != "DEUTNW1001NWBKDE" || stored.SettlementCents != 400_000 {
		t.Errorf("stored instruction = %+v, want LDCR DEUTNW1001NWBKDE and settlement 400000", stored)
	}
	// The audit trail carries exactly one registration event,
	// with the specified details and actor.
	events := readAuditOps(t, al)
	registered := findOp(events, "mt321_settlement_instruction_registered")
	if registered == nil {
		t.Fatalf("no mt321_settlement_instruction_registered event in %d events", len(events))
	}
	if registered.Actor != "ops" {
		t.Errorf("audit actor = %q, want ops", registered.Actor)
	}
	if registered.Details["msg_id"] != td.ID {
		t.Errorf("audit msg_id = %v, want %q", registered.Details["msg_id"], td.ID)
	}
	if registered.Details["common_ref"] != "DEUTNW1001NWBKDE" {
		t.Errorf("audit common_ref = %v, want DEUTNW1001NWBKDE", registered.Details["common_ref"])
	}
	if registered.Details["deposit_id"] != td.ID {
		t.Errorf("audit deposit_id = %v, want %q", registered.Details["deposit_id"], td.ID)
	}
	if countOp(events, "mt321_settlement_instruction_registered") != 1 {
		t.Errorf("mt321_settlement_instruction_registered count = %d, want 1", countOp(events, "mt321_settlement_instruction_registered"))
	}
}

func TestApplyMT321_RefusesLDCRMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.Replace(mt321ExportedDoc(t, l, td), ":20C::LDCR//DEUTNW1001NWBKDE", ":20C::LDCR//DEUTNW1001NWBKDF", 1)
	req := mt321ParsedInstruction(t, doc)

	if _, err := l.ApplyMT321(req, "ops"); !errors.Is(err, ErrMT321TermsMismatch) {
		t.Errorf("ApplyMT321: err = %v, want ErrMT321TermsMismatch", err)
	}
}

func TestApplyMT321_RefusesUnknownDeposit(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.ReplaceAll(mt321ExportedDoc(t, l, td), td.ID, "td-mt320-9999")
	req := mt321ParsedInstruction(t, doc)

	if _, err := l.ApplyMT321(req, "ops"); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("ApplyMT321: err = %v, want ErrTermDepositNotFound", err)
	}
}

func TestApplyMT321_RefusesWithdrawnDeposit(t *testing.T) {
	l, td := mt320TestLedger(t)
	// Export first: a withdrawn deposit cannot be exported, so
	// the manager's instruction must predate the withdrawal.
	req := mt321ParsedInstruction(t, mt321ExportedDoc(t, l, td))
	if _, err := l.WithdrawTermDeposit(td.ID, 0, time.Now()); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}

	if _, err := l.ApplyMT321(req, "ops"); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("ApplyMT321: err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestApplyMT321_RefusesTieredRate(t *testing.T) {
	l, td := mt320TestLedger(t)
	// A tiered-rate deposit cannot be exported, so the
	// instruction is hand-built from the flat fixture's parsed
	// terms, re-keyed to the tiered deposit.
	tiered, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-mt321-2002",
		Account:     "saver",
		AmountCents: 400_000,
		Currency:    "USD",
		TermDays:    90,
		RateTiers: []InterestTier{
			{MinCents: 0, RateBps: 500},
			{MinCents: 200_000, RateBps: 300},
		},
		OpenedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit tiered: %v", err)
	}
	req := mt321ParsedInstruction(t, mt321ExportedDoc(t, l, td))
	req.Reference = tiered.ID
	req.ContractNumber = tiered.ID
	req.CommonReference = "DEUTNW2002NWBKDE"

	if _, err := l.ApplyMT321(req, "ops"); !errors.Is(err, ErrInvalidMT321Export) {
		t.Errorf("ApplyMT321: err = %v, want ErrInvalidMT321Export", err)
	}
}

func TestApplyMT321_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l, td := mt321AuditLedger(t, al)
	doc := mt321ExportedDoc(t, l, td)
	req := mt321ParsedInstruction(t, doc)

	if _, err := l.ApplyMT321(req, "ops"); err != nil {
		t.Fatalf("first ApplyMT321: %v", err)
	}
	res, err := l.ApplyMT321(req, "ops")
	if err != nil {
		t.Fatalf("replay ApplyMT321: %v", err)
	}
	if !res.Duplicate {
		t.Error("Duplicate = false on replay, want true")
	}
	if res.Instruction == nil || res.Instruction.Reference != td.ID {
		t.Fatalf("replay Instruction = %+v, want reference %q", res.Instruction, td.ID)
	}
	// The replay emits no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "mt321_settlement_instruction_registered") != 1 {
		t.Errorf("mt321_settlement_instruction_registered count = %d, want 1", countOp(events, "mt321_settlement_instruction_registered"))
	}
	// The same SEME with different terms is a conflict, never a
	// silent overwrite.
	changed := strings.Replace(doc, ":92A::INTR//0,05", ":92A::INTR//0,06", 1)
	changedReq := mt321ParsedInstruction(t, changed)
	if _, err := l.ApplyMT321(changedReq, "ops"); !errors.Is(err, ErrMT321MsgIDConflict) {
		t.Errorf("ApplyMT321 with changed terms: err = %v, want ErrMT321MsgIDConflict", err)
	}
}

func TestApplyMT321_RefusesSettlementMismatch(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc := strings.Replace(mt321ExportedDoc(t, l, td), ":19A::SETT//USD4000,00", ":19A::SETT//USD4001,00", 1)
	req := mt321ParsedInstruction(t, doc)

	if _, err := l.ApplyMT321(req, "ops"); !errors.Is(err, ErrMT321TermsMismatch) {
		t.Errorf("ApplyMT321: err = %v, want ErrMT321TermsMismatch", err)
	}
}
