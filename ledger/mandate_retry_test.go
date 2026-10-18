package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// retryEnv is a ledger with an overdraft-protected, unfunded payer
// ("cust"), one active mandate ("m-rt", cap 5000 USD per debit,
// per-day), and audit + outbox wired to temp dirs.
type retryEnv struct {
	l        *Ledger
	al       *AuditLog
	ob       *Outbox
	auditDir string
	outDir   string
	closed   bool
}

func newRetryEnv(t *testing.T) *retryEnv {
	t.Helper()
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	outDir := filepath.Join(dir, "outbox")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatalf("mkdir audit: %v", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outbox: %v", err)
	}
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	ob, err := NewOutbox(outDir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	l := New(WithAuditLog(al), WithOutbox(ob))
	// The payer starts unfunded and protected: any debit overdraws.
	l.EnableOverdraftProtection("cust")
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-rt",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return &retryEnv{l: l, al: al, ob: ob, auditDir: auditDir, outDir: outDir}
}

func (e *retryEnv) close(t *testing.T) {
	t.Helper()
	if e.closed {
		return
	}
	e.closed = true
	if err := e.al.Close(); err != nil {
		t.Fatalf("audit close: %v", err)
	}
	if err := e.ob.Close(); err != nil {
		t.Fatalf("outbox close: %v", err)
	}
}

// setRetryPolicy installs the standard test policy: up to 3 retries,
// waiting 1 minute then 5 minutes.
func (e *retryEnv) setRetryPolicy(t *testing.T) {
	t.Helper()
	if err := e.l.SetMandateRetryPolicy("m-rt", MandateRetryPolicy{
		MaxRetries: 3,
		Backoff:    []time.Duration{time.Minute, 5 * time.Minute},
	}); err != nil {
		t.Fatalf("SetMandateRetryPolicy: %v", err)
	}
}

// failDebit drives one overdraft-failing debit under the retry mandate.
func (e *retryEnv) failDebit(t *testing.T, id, key string) error {
	t.Helper()
	_, err := e.l.DebitWithMandate("m-rt", MandateDebit{
		ID:             id,
		AmountCents:    1000,
		Currency:       "USD",
		IdempotencyKey: key,
	})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("DebitWithMandate: err = %v, want ErrAccountOverdraft", err)
	}
	return err
}

func TestMandateRetryPolicyValidation(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)

	invalid := []MandateRetryPolicy{
		{MaxRetries: 0, Backoff: []time.Duration{time.Minute}},
		{MaxRetries: -1, Backoff: []time.Duration{time.Minute}},
		{MaxRetries: 2, Backoff: nil},
		{MaxRetries: 2, Backoff: []time.Duration{}},
		{MaxRetries: 2, Backoff: []time.Duration{time.Minute, 0}},
		{MaxRetries: 2, Backoff: []time.Duration{-time.Second}},
	}
	for i, p := range invalid {
		if err := e.l.SetMandateRetryPolicy("m-rt", p); !errors.Is(err, ErrInvalidMandateRetryPolicy) {
			t.Errorf("case %d: err = %v, want ErrInvalidMandateRetryPolicy", i, err)
		}
	}
	valid := MandateRetryPolicy{MaxRetries: 2, Backoff: []time.Duration{time.Minute}}
	if err := e.l.SetMandateRetryPolicy("no-such-mandate", valid); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	if err := e.l.SetMandateRetryPolicy("m-rt", valid); err != nil {
		t.Fatalf("SetMandateRetryPolicy: %v", err)
	}
	got, ok := e.l.MandateRetryPolicyOf("m-rt")
	if !ok || got.MaxRetries != 2 || len(got.Backoff) != 1 {
		t.Fatalf("MandateRetryPolicyOf = %+v, %v", got, ok)
	}
	// The returned policy is a copy.
	got.Backoff[0] = time.Hour
	got2, _ := e.l.MandateRetryPolicyOf("m-rt")
	if got2.Backoff[0] != time.Minute {
		t.Error("mutating the returned policy changed the ledger")
	}
	if _, ok := e.l.MandateRetryPolicyOf("no-such-mandate"); ok {
		t.Error("MandateRetryPolicyOf(unknown) ok = true")
	}
	e.l.ClearMandateRetryPolicy("m-rt")
	if _, ok := e.l.MandateRetryPolicyOf("m-rt"); ok {
		t.Error("policy not cleared")
	}
	e.l.ClearMandateRetryPolicy("m-rt") // idempotent, no panic
}

func TestMandateRetrySchedulesOnInsufficientFunds(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	e.setRetryPolicy(t)

	before := time.Now()
	e.failDebit(t, "dbt-1", "key-1")

	pend := e.l.PendingMandateRetries()
	if len(pend) != 1 {
		t.Fatalf("pending retries = %d, want 1", len(pend))
	}
	rt := pend[0]
	if rt.ID != "dbt-1" || rt.MandateID != "m-rt" {
		t.Errorf("retry = %+v, want dbt-1/m-rt", rt)
	}
	if rt.NextAttempt != 1 {
		t.Errorf("NextAttempt = %d, want 1", rt.NextAttempt)
	}
	if rt.NextRetryAt.Before(before.Add(time.Minute)) || rt.NextRetryAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("NextRetryAt = %v, want ~now+1m", rt.NextRetryAt)
	}
	if rt.LastError == "" {
		t.Error("LastError is empty")
	}
	if rt.Policy.MaxRetries != 3 {
		t.Errorf("scheduled policy MaxRetries = %d, want 3 (snapshot at schedule time)", rt.Policy.MaxRetries)
	}
	// The failed debit booked nothing.
	if _, ok := e.l.entries["dbt-1"]; ok {
		t.Error("failed debit booked a journal entry")
	}
	if n := len(e.l.mandateDebits["m-rt"]); n != 0 {
		t.Errorf("mandate debits = %d, want 0", n)
	}
	// Re-driving the same debit ID does not double-schedule.
	e.failDebit(t, "dbt-1", "key-1")
	if len(e.l.PendingMandateRetries()) != 1 {
		t.Error("re-driven failure double-scheduled the retry")
	}
}

func TestMandateRetrySweepSucceeds(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	e.setRetryPolicy(t)
	e.failDebit(t, "dbt-1", "key-1")

	// Fund the payer after the failure.
	if _, _, err := e.l.Post(JournalEntry{ID: "fund-1", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}

	// A sweep before the backoff elapses fires nothing.
	if runs := e.l.SweepMandateRetries(time.Now()); len(runs) != 0 {
		t.Errorf("early sweep fired %d runs, want 0", len(runs))
	}

	runs := e.l.SweepMandateRetries(time.Now().Add(2 * time.Minute))
	if len(runs) != 1 {
		t.Fatalf("sweep runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Outcome != MandateRetryOutcomeSucceeded {
		t.Errorf("outcome = %q, want succeeded", run.Outcome)
	}
	if run.Attempt != 1 || run.DebitID != "dbt-1" || run.EntryID == "" {
		t.Errorf("run = %+v", run)
	}
	// The attempt used an independent idempotency key: the original key
	// was never indexed (attempt 0 booked nothing).
	entry, ok := e.l.byKey["key-1#attempt-1"]
	if !ok {
		t.Fatal("attempt key key-1#attempt-1 not indexed")
	}
	if entry.IdempotencyKey != "key-1#attempt-1" {
		t.Errorf("entry key = %q", entry.IdempotencyKey)
	}
	if _, ok := e.l.byKey["key-1"]; ok {
		t.Error("original key indexed though attempt 0 booked nothing")
	}
	// The retry is finalized.
	if len(e.l.PendingMandateRetries()) != 0 {
		t.Error("retry not removed after success")
	}
	if got := e.l.Balance("cust"); got != 99000 {
		t.Errorf("cust balance = %d, want 99000", got)
	}
	// A second sweep fires nothing.
	if runs := e.l.SweepMandateRetries(time.Now().Add(time.Hour)); len(runs) != 0 {
		t.Errorf("second sweep fired %d runs, want 0", len(runs))
	}

	e.close(t)
	byOp := auditEventsByOp(t, e.auditDir)
	if len(byOp["mandate_debit_retry_scheduled"]) != 1 {
		t.Errorf("scheduled events = %d, want 1", len(byOp["mandate_debit_retry_scheduled"]))
	}
	succeeded := byOp["mandate_debit_retry_succeeded"]
	if len(succeeded) != 1 {
		t.Fatalf("succeeded events = %d, want 1", len(succeeded))
	}
	if succeeded[0].Details["attempt"] != float64(1) || succeeded[0].Details["debit_id"] != "dbt-1" {
		t.Errorf("succeeded event = %+v", succeeded[0].Details)
	}
}

func TestMandateRetryRescheduleAndExhaustion(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	if err := e.l.SetMandateRetryPolicy("m-rt", MandateRetryPolicy{
		MaxRetries: 2,
		Backoff:    []time.Duration{time.Minute, 5 * time.Minute},
	}); err != nil {
		t.Fatalf("SetMandateRetryPolicy: %v", err)
	}
	t0 := time.Now()
	e.failDebit(t, "dbt-x", "key-x")

	// Attempt 1 still fails (payer unfunded) → rescheduled.
	runs := e.l.SweepMandateRetries(t0.Add(90 * time.Second))
	if len(runs) != 1 {
		t.Fatalf("sweep 1 runs = %d, want 1", len(runs))
	}
	if runs[0].Outcome != MandateRetryOutcomeRescheduled || runs[0].Attempt != 1 {
		t.Errorf("run 1 = %+v, want rescheduled attempt 1", runs[0])
	}
	pend := e.l.PendingMandateRetries()
	if len(pend) != 1 || pend[0].NextAttempt != 2 {
		t.Fatalf("pending = %+v, want one retry with NextAttempt 2", pend)
	}
	// The backoff table advanced: the wait is now 5 minutes.
	if want := t0.Add(90*time.Second + 5*time.Minute); pend[0].NextRetryAt.Before(want.Add(-5*time.Second)) || pend[0].NextRetryAt.After(want.Add(5*time.Second)) {
		t.Errorf("NextRetryAt = %v, want ~%v", pend[0].NextRetryAt, want)
	}

	// Attempt 2 fails with no attempts left → exhausted.
	runs = e.l.SweepMandateRetries(t0.Add(90*time.Second + 6*time.Minute))
	if len(runs) != 1 {
		t.Fatalf("sweep 2 runs = %d, want 1", len(runs))
	}
	if runs[0].Outcome != MandateRetryOutcomeExhausted || runs[0].Attempt != 2 {
		t.Errorf("run 2 = %+v, want exhausted attempt 2", runs[0])
	}
	if runs[0].Error == "" {
		t.Error("exhausted run has no error")
	}
	if len(e.l.PendingMandateRetries()) != 0 {
		t.Error("exhausted retry not removed")
	}
	// Nothing was ever booked.
	if _, ok := e.l.entries["dbt-x"]; ok {
		t.Error("failed debit booked a journal entry")
	}

	e.close(t)
	byOp := auditEventsByOp(t, e.auditDir)
	for op, want := range map[string]int{
		"mandate_debit_retry_scheduled": 1,
		"mandate_debit_retry_attempt":   1,
		"mandate_debit_retry_exhausted": 1,
	} {
		if len(byOp[op]) != want {
			t.Errorf("audit %s events = %d, want %d", op, len(byOp[op]), want)
		}
	}
	if got := byOp["mandate_debit_retry_exhausted"][0].Details["attempts"]; got != float64(2) {
		t.Errorf("exhausted attempts = %v, want 2", got)
	}
	// The exhaustion fired the mandate event for downstream dunning.
	evs, _, corrupt, err := ReadOutboxEvents(e.outDir, OutboxQuery{Types: []string{OutboxMandateRetryExhausted}, Limit: 100})
	if err != nil {
		t.Fatalf("ReadOutboxEvents: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d", corrupt)
	}
	if len(evs) != 1 {
		t.Fatalf("mandate_retry_exhausted outbox events = %d, want 1", len(evs))
	}
	if evs[0].Details["debit_id"] != "dbt-x" || evs[0].Details["mandate_id"] != "m-rt" {
		t.Errorf("exhausted event = %+v", evs[0].Details)
	}
}

func TestMandateRetrySecondAttemptKey(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	if err := e.l.SetMandateRetryPolicy("m-rt", MandateRetryPolicy{
		MaxRetries: 2,
		Backoff:    []time.Duration{time.Minute},
	}); err != nil {
		t.Fatalf("SetMandateRetryPolicy: %v", err)
	}
	t0 := time.Now()
	e.failDebit(t, "dbt-2", "key-2")

	// Attempt 1 fails while unfunded.
	runs := e.l.SweepMandateRetries(t0.Add(90 * time.Second))
	if len(runs) != 1 || runs[0].Outcome != MandateRetryOutcomeRescheduled {
		t.Fatalf("run 1 = %+v, want rescheduled", runs)
	}
	// Fund, then attempt 2 succeeds under its own key.
	if _, _, err := e.l.Post(JournalEntry{ID: "fund-2", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	runs = e.l.SweepMandateRetries(t0.Add(3 * time.Minute))
	if len(runs) != 1 || runs[0].Outcome != MandateRetryOutcomeSucceeded || runs[0].Attempt != 2 {
		t.Fatalf("run 2 = %+v, want succeeded attempt 2", runs)
	}
	if _, ok := e.l.byKey["key-2#attempt-2"]; !ok {
		t.Error("attempt-2 key not indexed")
	}
	if _, ok := e.l.byKey["key-2#attempt-1"]; ok {
		t.Error("attempt-1 key indexed though attempt 1 booked nothing")
	}
}

func TestMandateRetryTerminalFailure(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	e.setRetryPolicy(t)

	// A validation failure never schedules: the amount exceeds the cap.
	_, err := e.l.DebitWithMandate("m-rt", MandateDebit{ID: "dbt-big", AmountCents: 99999, Currency: "USD"})
	if !errors.Is(err, ErrMandateAmountExceeded) {
		t.Fatalf("err = %v, want ErrMandateAmountExceeded", err)
	}
	if len(e.l.PendingMandateRetries()) != 0 {
		t.Error("validation failure scheduled a retry")
	}

	// A pending retry whose mandate is suspended drops terminally.
	e.failDebit(t, "dbt-3", "key-3")
	if _, err := e.l.SuspendMandate("m-rt"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	runs := e.l.SweepMandateRetries(time.Now().Add(time.Hour))
	if len(runs) != 1 {
		t.Fatalf("sweep runs = %d, want 1", len(runs))
	}
	if runs[0].Outcome != MandateRetryOutcomeTerminal {
		t.Errorf("outcome = %q, want terminal", runs[0].Outcome)
	}
	if len(e.l.PendingMandateRetries()) != 0 {
		t.Error("terminal retry not removed")
	}

	e.close(t)
	byOp := auditEventsByOp(t, e.auditDir)
	if len(byOp["mandate_debit_retry_terminal"]) != 1 {
		t.Errorf("terminal events = %d, want 1", len(byOp["mandate_debit_retry_terminal"]))
	}
}

func TestMandateRetryNoPolicyNoSchedule(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	// No policy installed: the retryable failure is returned as-is.
	e.failDebit(t, "dbt-np", "key-np")
	if len(e.l.PendingMandateRetries()) != 0 {
		t.Error("failure scheduled a retry without a policy")
	}
}

func TestCancelMandateRetries(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	e.setRetryPolicy(t)
	e.failDebit(t, "dbt-a", "key-a")
	e.failDebit(t, "dbt-b", "key-b")
	if len(e.l.PendingMandateRetries()) != 2 {
		t.Fatalf("pending = %d, want 2", len(e.l.PendingMandateRetries()))
	}
	if n := e.l.CancelMandateRetries("m-rt"); n != 2 {
		t.Errorf("cancelled = %d, want 2", n)
	}
	if len(e.l.PendingMandateRetries()) != 0 {
		t.Error("retries not cancelled")
	}
	if n := e.l.CancelMandateRetries("m-rt"); n != 0 {
		t.Errorf("second cancel = %d, want 0", n)
	}
	// The sweep fires nothing after cancellation.
	if runs := e.l.SweepMandateRetries(time.Now().Add(time.Hour)); len(runs) != 0 {
		t.Errorf("sweep fired %d runs after cancel, want 0", len(runs))
	}
}

func TestMandateRetryDailyLimitIsRetryable(t *testing.T) {
	e := newRetryEnv(t)
	defer e.close(t)
	e.setRetryPolicy(t)
	// Fund the payer, then install a daily limit the debit exceeds.
	if _, _, err := e.l.Post(JournalEntry{ID: "fund-3", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if err := e.l.SetDailyLimit("cust", "USD", 100); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}
	_, err := e.l.DebitWithMandate("m-rt", MandateDebit{ID: "dbt-dl", AmountCents: 1000, Currency: "USD"})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("err = %v, want ErrDailyLimitExceeded", err)
	}
	if len(e.l.PendingMandateRetries()) != 1 {
		t.Error("daily-limit failure did not schedule a retry")
	}
}
