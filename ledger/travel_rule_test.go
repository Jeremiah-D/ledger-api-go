package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// seedTravelLedger returns a ledger with a funded payer and holder
// names registered for both the payer ("payer") and the payee
// ("payee"), unless the caller passes "" to leave that side
// unregistered.
func seedTravelLedger(t *testing.T, payerName, payeeName string) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "seed-travel-1", DebitAccount: "payer", CreditAccount: "funding",
		AmountCents: 500000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Post seed: %v", err)
	}
	if payerName != "" {
		if err := l.SetAccountHolderName("payer", payerName); err != nil {
			t.Fatalf("SetAccountHolderName payer: %v", err)
		}
	}
	if payeeName != "" {
		if err := l.SetAccountHolderName("payee", payeeName); err != nil {
			t.Fatalf("SetAccountHolderName payee: %v", err)
		}
	}
	return l
}

func postTravelTransfer(t *testing.T, l *Ledger, id string, amount int64, originatorID string, key string) (TransferReceipt, error) {
	t.Helper()
	return l.PostTransfer(Transfer{
		ID:                     id,
		From:                   "payer",
		To:                     "payee",
		AmountCents:            amount,
		CrossBorder:            true,
		TravelRuleOriginatorID: originatorID,
		IdempotencyKey:         key,
		CreatedAt:              time.Now(),
	})
}

func TestAssessTravelRule(t *testing.T) {
	complete := TravelRuleInfo{
		OriginatorName: "Alice Originator", OriginatorAccount: "payer",
		OriginatorAddressOrID: "1 Main Street", BeneficiaryName: "Bob Beneficiary",
		BeneficiaryAccount: "payee", AmountMinor: 150000,
	}
	cases := []struct {
		name  string
		info  TravelRuleInfo
		want  TravelRuleAssessment
	}{
		{
			"complete above threshold",
			complete,
			TravelRuleAssessment{Complete: true, AboveThreshold: true,
				Present: []string{TravelRuleFieldOriginatorName, TravelRuleFieldOriginatorAccount, TravelRuleFieldOriginatorAddressOrID, TravelRuleFieldBeneficiaryName, TravelRuleFieldBeneficiaryAccount}},
		},
		{
			"identifier satisfies originator third leg",
			TravelRuleInfo{OriginatorName: "Alice", OriginatorAccount: "payer", OriginatorAddressOrID: "ID-12345", BeneficiaryName: "Bob", BeneficiaryAccount: "payee", AmountMinor: 150000},
			TravelRuleAssessment{Complete: true, AboveThreshold: true,
				Present: []string{TravelRuleFieldOriginatorName, TravelRuleFieldOriginatorAccount, TravelRuleFieldOriginatorAddressOrID, TravelRuleFieldBeneficiaryName, TravelRuleFieldBeneficiaryAccount}},
		},
		{
			"missing originator address-or-id and beneficiary name",
			TravelRuleInfo{OriginatorName: "Alice", OriginatorAccount: "payer", BeneficiaryAccount: "payee", AmountMinor: 150000},
			TravelRuleAssessment{Complete: false, AboveThreshold: true,
				Missing: []string{TravelRuleFieldOriginatorAddressOrID, TravelRuleFieldBeneficiaryName},
				Present: []string{TravelRuleFieldOriginatorName, TravelRuleFieldOriginatorAccount, TravelRuleFieldBeneficiaryAccount}},
		},
		{
			"incomplete below threshold still reports present fields",
			TravelRuleInfo{OriginatorName: "Alice", OriginatorAccount: "payer", BeneficiaryAccount: "payee", AmountMinor: 50000},
			TravelRuleAssessment{Complete: false, AboveThreshold: false,
				Missing: []string{TravelRuleFieldOriginatorAddressOrID, TravelRuleFieldBeneficiaryName},
				Present: []string{TravelRuleFieldOriginatorName, TravelRuleFieldOriginatorAccount, TravelRuleFieldBeneficiaryAccount}},
		},
		{
			"exactly at threshold is not above",
			TravelRuleInfo{OriginatorName: "Alice", OriginatorAccount: "payer", OriginatorAddressOrID: "ID-1", BeneficiaryName: "Bob", BeneficiaryAccount: "payee", AmountMinor: DefaultTravelRuleThresholdMinorUnits},
			TravelRuleAssessment{Complete: true, AboveThreshold: false,
				Present: []string{TravelRuleFieldOriginatorName, TravelRuleFieldOriginatorAccount, TravelRuleFieldOriginatorAddressOrID, TravelRuleFieldBeneficiaryName, TravelRuleFieldBeneficiaryAccount}},
		},
		{
			"blank strings count as missing",
			TravelRuleInfo{OriginatorName: "   ", OriginatorAccount: "payer", OriginatorAddressOrID: "ID-1", BeneficiaryName: "Bob", BeneficiaryAccount: "payee", AmountMinor: 150000},
			TravelRuleAssessment{Complete: false, AboveThreshold: true,
				Missing: []string{TravelRuleFieldOriginatorName},
				Present: []string{TravelRuleFieldOriginatorAccount, TravelRuleFieldOriginatorAddressOrID, TravelRuleFieldBeneficiaryName, TravelRuleFieldBeneficiaryAccount}},
		},
	}
	for _, c := range cases {
		got := AssessTravelRule(c.info, DefaultTravelRulePolicy())
		if got.Complete != c.want.Complete || got.AboveThreshold != c.want.AboveThreshold ||
			strings.Join(got.Missing, ",") != strings.Join(c.want.Missing, ",") ||
			strings.Join(got.Present, ",") != strings.Join(c.want.Present, ",") {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestTravelRulePolicyValidation(t *testing.T) {
	if err := DefaultTravelRulePolicy().validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	for _, p := range []TravelRulePolicy{{ThresholdMinorUnits: 0}, {ThresholdMinorUnits: -100}} {
		if err := p.validate(); !errors.Is(err, ErrInvalidTravelRulePolicy) {
			t.Errorf("policy %+v: err = %v, want ErrInvalidTravelRulePolicy", p, err)
		}
	}
	l := New()
	if err := l.SetTravelRulePolicy(TravelRulePolicy{ThresholdMinorUnits: 0}); !errors.Is(err, ErrInvalidTravelRulePolicy) {
		t.Fatalf("SetTravelRulePolicy(0): err = %v, want ErrInvalidTravelRulePolicy", err)
	}
	custom := TravelRulePolicy{ThresholdMinorUnits: 500}
	if err := l.SetTravelRulePolicy(custom); err != nil {
		t.Fatalf("SetTravelRulePolicy: %v", err)
	}
	if got := l.TravelRulePolicy(); got != custom {
		t.Fatalf("policy = %+v, want %+v", got, custom)
	}
	// A zero policy resolves to the default instead of forcing every
	// transfer: 100001 is above the default 100000 threshold.
	if got := AssessTravelRule(TravelRuleInfo{AmountMinor: 100001}, TravelRulePolicy{}); !got.AboveThreshold {
		t.Error("zero policy: 100001 should be above the default threshold")
	}
}

func TestCheckTravelRuleFailClosedAboveThreshold(t *testing.T) {
	l := seedTravelLedger(t, "Alice Originator", "Bob Beneficiary")
	info := TravelRuleInfo{
		OriginatorName: "Alice Originator", OriginatorAccount: "payer",
		BeneficiaryName: "Bob Beneficiary", BeneficiaryAccount: "payee",
		AmountMinor: 150000, // above threshold, no address-or-ID
	}
	if _, err := l.CheckTravelRule(info); !errors.Is(err, ErrTravelRuleIncomplete) {
		t.Fatalf("CheckTravelRule: err = %v, want ErrTravelRuleIncomplete", err)
	}
	// PII discipline: the error names the missing field, never values.
	if _, err := l.CheckTravelRule(info); strings.Contains(err.Error(), "Alice") || strings.Contains(err.Error(), "Bob") {
		t.Errorf("error leaks PII values: %v", err)
	}
	below := info
	below.AmountMinor = 50000
	if _, err := l.CheckTravelRule(below); err != nil {
		t.Errorf("below-threshold incomplete CheckTravelRule: err = %v, want nil", err)
	}
}

func TestPostTransferTravelRuleCompleteBooks(t *testing.T) {
	l := seedTravelLedger(t, "Alice Originator", "Bob Beneficiary")
	receipt, err := postTravelTransfer(t, l, "t-travel-ok", 150000, "1 Main Street", "k-travel-ok")
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.TravelRule == nil || !receipt.TravelRule.Complete || !receipt.TravelRule.AboveThreshold {
		t.Fatalf("TravelRule disclosure = %+v, want {complete above_threshold}", receipt.TravelRule)
	}
	if bal := l.Balance("payee"); bal != 150000 {
		t.Errorf("payee balance = %d, want 150000", bal)
	}
}

func TestPostTransferTravelRuleAboveThresholdRejected(t *testing.T) {
	l := seedTravelLedger(t, "Alice Originator", "Bob Beneficiary")
	if _, err := postTravelTransfer(t, l, "t-travel-missing-id", 150000, "", "k-travel-missing-id"); !errors.Is(err, ErrTravelRuleIncomplete) {
		t.Fatalf("missing originator ID: err = %v, want ErrTravelRuleIncomplete", err)
	}
	if bal := l.Balance("payee"); bal != 0 {
		t.Errorf("rejected transfer booked: payee balance = %d, want 0", bal)
	}
	// Unregistered beneficiary holder name is a missing field too.
	l2 := seedTravelLedger(t, "Alice Originator", "")
	if _, err := postTravelTransfer(t, l2, "t-travel-missing-name", 150000, "1 Main Street", "k-travel-missing-name"); !errors.Is(err, ErrTravelRuleIncomplete) {
		t.Fatalf("missing beneficiary name: err = %v, want ErrTravelRuleIncomplete", err)
	}
}

func TestPostTransferTravelRuleBelowThresholdBooksIncomplete(t *testing.T) {
	l := seedTravelLedger(t, "Alice Originator", "Bob Beneficiary")
	receipt, err := postTravelTransfer(t, l, "t-travel-below", 50000, "", "k-travel-below")
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.TravelRule == nil || receipt.TravelRule.Complete || receipt.TravelRule.AboveThreshold {
		t.Fatalf("TravelRule disclosure = %+v, want {incomplete below_threshold}", receipt.TravelRule)
	}
	if bal := l.Balance("payee"); bal != 50000 {
		t.Errorf("payee balance = %d, want 50000", bal)
	}
}

func TestPostTransferTravelRuleSkippedWhenDomestic(t *testing.T) {
	l := seedTravelLedger(t, "", "")
	receipt, err := l.PostTransfer(Transfer{
		ID: "t-travel-domestic", From: "payer", To: "payee",
		AmountCents: 150000, IdempotencyKey: "k-travel-domestic", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.TravelRule != nil {
		t.Errorf("domestic transfer TravelRule = %+v, want nil", receipt.TravelRule)
	}
}

func TestPostTransferTravelRuleCustomThreshold(t *testing.T) {
	l := seedTravelLedger(t, "Alice Originator", "Bob Beneficiary")
	if err := l.SetTravelRulePolicy(TravelRulePolicy{ThresholdMinorUnits: 500}); err != nil {
		t.Fatalf("SetTravelRulePolicy: %v", err)
	}
	if _, err := postTravelTransfer(t, l, "t-travel-custom", 1000, "", "k-travel-custom"); !errors.Is(err, ErrTravelRuleIncomplete) {
		t.Fatalf("custom threshold: err = %v, want ErrTravelRuleIncomplete", err)
	}
	if _, err := postTravelTransfer(t, l, "t-travel-custom-below", 400, "", "k-travel-custom-below"); err != nil {
		t.Fatalf("below custom threshold: err = %v, want nil", err)
	}
}

func TestPostTransferTravelRuleOriginatorIDTooLong(t *testing.T) {
	l := seedTravelLedger(t, "Alice Originator", "Bob Beneficiary")
	long := strings.Repeat("x", MaxTravelRuleOriginatorIDRunes+1)
	if _, err := postTravelTransfer(t, l, "t-travel-long-id", 1000, long, "k-travel-long-id"); !errors.Is(err, ErrInvalidTravelRuleOriginatorID) {
		t.Fatalf("overlong originator ID: err = %v, want ErrInvalidTravelRuleOriginatorID", err)
	}
}

func TestDryRunTransferTravelRule(t *testing.T) {
	l := seedTravelLedger(t, "Alice Originator", "Bob Beneficiary")
	_, err := l.DryRunTransfer(Transfer{
		ID: "t-travel-dryrun", From: "payer", To: "payee",
		AmountCents: 150000, CrossBorder: true, CreatedAt: time.Now(),
	})
	if !errors.Is(err, ErrTravelRuleIncomplete) {
		t.Fatalf("dry-run error = %v, want ErrTravelRuleIncomplete", err)
	}
	// The dry run books nothing on the live ledger.
	if bal := l.Balance("payee"); bal != 0 {
		t.Errorf("dry run booked: payee balance = %d, want 0", bal)
	}
}
