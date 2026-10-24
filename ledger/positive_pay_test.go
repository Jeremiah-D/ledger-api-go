package ledger

import (
	"errors"
	"testing"
)

func mustRegisterCheck(t *testing.T, l *Ledger, c IssuedCheck) {
	t.Helper()
	if err := l.RegisterIssuedCheck(c); err != nil {
		t.Fatalf("RegisterIssuedCheck: %v", err)
	}
}

func issuedCheck(number string, amount int64, payee string) IssuedCheck {
	return IssuedCheck{
		Account: "checking", Number: number, AmountMinor: amount,
		Currency: "USD", Payee: payee, IssueDate: "2026-10-20",
	}
}

func presentedCheck(number string, amount int64, payee string) PresentedCheck {
	return PresentedCheck{
		Account: "checking", Number: number, AmountMinor: amount,
		Currency: "USD", Payee: payee,
	}
}

func TestPositivePayFullMatchClears(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	res, err := l.PresentCheck(presentedCheck("1001", 50000, "acme  supplies"))
	if err != nil {
		t.Fatalf("PresentCheck: %v", err)
	}
	if res.Disposition != PositivePayCleared || res.Exception != nil {
		t.Fatalf("disposition = %q exception = %+v, want cleared/nil", res.Disposition, res.Exception)
	}
	// Re-presentment of a cleared check is idempotently cleared.
	res, err = l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayCleared {
		t.Fatalf("re-present cleared: disposition = %q err = %v, want cleared", res.Disposition, err)
	}
	if _, ok := l.GetPositivePayException("checking", "1001"); ok {
		t.Error("cleared check left an exception behind")
	}
}

func TestPositivePayMismatchesEnterExceptionQueue(t *testing.T) {
	cases := []struct {
		name   string
		issued IssuedCheck
		pres   PresentedCheck
		reason string
	}{
		{"amount mismatch", issuedCheck("2001", 50000, "Acme"), presentedCheck("2001", 50001, "Acme"), PositivePayReasonAmountMismatch},
		{"payee mismatch", issuedCheck("2002", 50000, "Acme"), presentedCheck("2002", 50000, "Globex"), PositivePayReasonPayeeMismatch},
		{"currency mismatch", issuedCheck("2003", 50000, "Acme"), PresentedCheck{Account: "checking", Number: "2003", AmountMinor: 50000, Currency: "EUR", Payee: "Acme"}, PositivePayReasonCurrencyMismatch},
		{"unregistered", IssuedCheck{}, presentedCheck("2999", 1000, "Nobody"), PositivePayReasonUnregistered},
	}
	for _, c := range cases {
		l := New()
		if c.issued.Number != "" {
			mustRegisterCheck(t, l, c.issued)
		}
		res, err := l.PresentCheck(c.pres)
		if err != nil {
			t.Fatalf("%s: PresentCheck: %v", c.name, err)
		}
		if res.Disposition != PositivePayDispositionException || res.Exception == nil {
			t.Fatalf("%s: disposition = %q, want exception with entry", c.name, res.Disposition)
		}
		if res.Exception.Reason != c.reason || res.Exception.Status != PositivePayExceptionPending {
			t.Fatalf("%s: exception = %+v, want reason %q pending", c.name, res.Exception, c.reason)
		}
		// Re-presentment of a pending exception returns the same
		// exception, not a duplicate entry.
		res2, err := l.PresentCheck(c.pres)
		if err != nil || res2.Disposition != PositivePayDispositionException {
			t.Fatalf("%s: re-present pending: disposition = %q err = %v", c.name, res2.Disposition, err)
		}
		if got := l.ListPositivePayExceptions(); len(got) != 1 {
			t.Fatalf("%s: exceptions = %d, want 1", c.name, len(got))
		}
	}
}

func TestPositivePayDecisions(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("3001", 50000, "Acme"))
	mustRegisterCheck(t, l, issuedCheck("3002", 70000, "Globex"))
	if _, err := l.PresentCheck(presentedCheck("3001", 49999, "Acme")); err != nil {
		t.Fatalf("PresentCheck 3001: %v", err)
	}
	if _, err := l.PresentCheck(presentedCheck("3002", 70000, "Initech")); err != nil {
		t.Fatalf("PresentCheck 3002: %v", err)
	}
	if err := l.DecidePositivePayException("checking", "3001", PositivePayDecisionPay); err != nil {
		t.Fatalf("Decide pay: %v", err)
	}
	if err := l.DecidePositivePayException("checking", "3002", PositivePayDecisionReturn); err != nil {
		t.Fatalf("Decide return: %v", err)
	}
	// A paid exception clears on re-presentment; a returned one
	// stays returned — neither books through this module, the caller
	// posts only cleared checks.
	if res, _ := l.PresentCheck(presentedCheck("3001", 49999, "Acme")); res.Disposition != PositivePayCleared {
		t.Errorf("paid exception re-present: disposition = %q, want cleared", res.Disposition)
	}
	if res, _ := l.PresentCheck(presentedCheck("3002", 70000, "Initech")); res.Disposition != PositivePayReturned {
		t.Errorf("returned exception re-present: disposition = %q, want returned", res.Disposition)
	}
	// Deciding again, deciding a missing check, or an unknown
	// decision are all errors.
	if err := l.DecidePositivePayException("checking", "3001", PositivePayDecisionPay); !errors.Is(err, ErrPositivePayExceptionNotPending) {
		t.Errorf("re-decide: err = %v, want ErrPositivePayExceptionNotPending", err)
	}
	if err := l.DecidePositivePayException("checking", "3999", PositivePayDecisionPay); !errors.Is(err, ErrPositivePayExceptionNotFound) {
		t.Errorf("missing exception: err = %v, want ErrPositivePayExceptionNotFound", err)
	}
	if err := l.DecidePositivePayException("checking", "3002", "hold"); !errors.Is(err, ErrPositivePayInvalidDecision) {
		t.Errorf("bad decision: err = %v, want ErrPositivePayInvalidDecision", err)
	}
}

func TestPositivePayRegistrationValidation(t *testing.T) {
	l := New()
	if err := l.RegisterIssuedCheck(issuedCheck("4001", 50000, "Acme")); err != nil {
		t.Fatalf("RegisterIssuedCheck: %v", err)
	}
	if err := l.RegisterIssuedCheck(issuedCheck("4001", 60000, "Other")); !errors.Is(err, ErrPositivePayDuplicateCheck) {
		t.Errorf("duplicate register: err = %v, want ErrPositivePayDuplicateCheck", err)
	}
	// Same number on a different account is a different check.
	if err := l.RegisterIssuedCheck(IssuedCheck{Account: "savings", Number: "4001", AmountMinor: 1000, Currency: "USD", Payee: "Acme", IssueDate: "2026-10-20"}); err != nil {
		t.Errorf("same number other account: err = %v, want nil", err)
	}
	for _, bad := range []IssuedCheck{
		{Account: "", Number: "4002", AmountMinor: 1000, Currency: "USD", Payee: "Acme", IssueDate: "2026-10-20"},
		{Account: "checking", Number: "", AmountMinor: 1000, Currency: "USD", Payee: "Acme", IssueDate: "2026-10-20"},
		{Account: "checking", Number: "4002", AmountMinor: 0, Currency: "USD", Payee: "Acme", IssueDate: "2026-10-20"},
		{Account: "checking", Number: "4002", AmountMinor: 1000, Currency: "", Payee: "Acme", IssueDate: "2026-10-20"},
		{Account: "checking", Number: "4002", AmountMinor: 1000, Currency: "USD", Payee: "", IssueDate: "2026-10-20"},
		{Account: "checking", Number: "4002", AmountMinor: 1000, Currency: "USD", Payee: "Acme", IssueDate: ""},
	} {
		if err := l.RegisterIssuedCheck(bad); !errors.Is(err, ErrInvalidIssuedCheck) {
			t.Errorf("issued %+v: err = %v, want ErrInvalidIssuedCheck", bad, err)
		}
	}
	if _, err := l.PresentCheck(PresentedCheck{Account: "checking", Number: "4001", AmountMinor: 0, Currency: "USD", Payee: "Acme"}); !errors.Is(err, ErrInvalidPresentedCheck) {
		t.Errorf("zero-amount present: err = %v, want ErrInvalidPresentedCheck", err)
	}
}

func TestPositivePayExceptionListingSorted(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("5002", 1000, "Acme"))
	mustRegisterCheck(t, l, issuedCheck("5001", 1000, "Acme"))
	if _, err := l.PresentCheck(presentedCheck("5002", 999, "Acme")); err != nil {
		t.Fatalf("PresentCheck: %v", err)
	}
	if _, err := l.PresentCheck(presentedCheck("5001", 999, "Acme")); err != nil {
		t.Fatalf("PresentCheck: %v", err)
	}
	if _, err := l.PresentCheck(presentedCheck("5999", 1000, "Nobody")); err != nil {
		t.Fatalf("PresentCheck: %v", err)
	}
	got := l.ListPositivePayExceptions()
	if len(got) != 3 || got[0].Number != "5001" || got[1].Number != "5002" || got[2].Number != "5999" {
		t.Fatalf("exceptions order = %+v, want 5001/5002/5999", got)
	}
	// Get returns a copy: mutating it must not mutate the queue.
	if exc, ok := l.GetPositivePayException("checking", "5001"); !ok {
		t.Fatal("GetPositivePayException missing")
	} else {
		exc.Status = PositivePayExceptionPaid
		if again, _ := l.GetPositivePayException("checking", "5001"); again.Status != PositivePayExceptionPending {
			t.Errorf("Get copy mutated queue: status = %q", again.Status)
		}
	}
}
