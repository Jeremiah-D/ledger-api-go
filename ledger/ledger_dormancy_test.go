package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func postDormantSeed(t *testing.T, l *Ledger, id, debit, credit string, amount int64, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit), AmountCents: amount, CreatedAt: at}); err != nil {
		t.Fatalf("seed Post %s: %v", id, err)
	}
}

func TestDetectDormantMarksInactive(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "quiet-a", "quiet-b", 100, old)
	postDormantSeed(t, l, "recent-1", "active-a", "active-b", 100, now.AddDate(0, 0, -5))

	report, err := l.DetectDormant(30)
	if err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	if report.InactivityDays != 30 {
		t.Fatalf("report inactivity days = %d, want 30", report.InactivityDays)
	}
	got := map[AccountID]bool{}
	for _, m := range report.Marked {
		got[m.Account] = true
		if m.LastActivity.IsZero() || m.MarkedAt.IsZero() {
			t.Fatalf("mark record %+v has zero timestamps", m)
		}
	}
	// Both legs of the old entry are quiet; the recent entry's legs are
	// not. Accounts with no journal history are never marked.
	for _, a := range []AccountID{"quiet-a", "quiet-b"} {
		if !got[a] {
			t.Fatalf("account %q not marked dormant", a)
		}
	}
	for _, a := range []AccountID{"active-a", "active-b"} {
		if got[a] {
			t.Fatalf("account %q wrongly marked dormant", a)
		}
	}
	// Sorted by account.
	for i := 1; i < len(report.Marked); i++ {
		if report.Marked[i-1].Account >= report.Marked[i].Account {
			t.Fatalf("marks not sorted: %+v", report.Marked)
		}
	}
	if !l.IsDormant("quiet-a") || !l.IsDormant("quiet-b") {
		t.Fatal("IsDormant false for marked accounts")
	}
	if l.IsDormant("active-a") {
		t.Fatal("IsDormant true for active account")
	}
	// Re-running marks nothing new and re-audits nothing.
	again, err := l.DetectDormant(30)
	if err != nil {
		t.Fatalf("second DetectDormant: %v", err)
	}
	if len(again.Marked) != 0 {
		t.Fatalf("second scan marked %d accounts, want 0", len(again.Marked))
	}
}

func TestDetectDormantValidation(t *testing.T) {
	l := New()
	for _, days := range []int{0, -1, -30} {
		if _, err := l.DetectDormant(days); !errors.Is(err, ErrInvalidInactivityDays) {
			t.Fatalf("days=%d err = %v, want ErrInvalidInactivityDays", days, err)
		}
	}
}

func TestDormantRejectsPost(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "counterparty", 100, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	// Either leg dormant rejects.
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "sleeper", CreditAccount: "fresh", AmountCents: 10}); !errors.Is(err, ErrAccountDormant) {
		t.Fatalf("dormant debit err = %v, want ErrAccountDormant", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "p2", DebitAccount: "fresh", CreditAccount: "sleeper", AmountCents: 10}); !errors.Is(err, ErrAccountDormant) {
		t.Fatalf("dormant credit err = %v, want ErrAccountDormant", err)
	}
	// Unrelated accounts still post.
	if _, _, err := l.Post(JournalEntry{ID: "p3", DebitAccount: "fresh", CreditAccount: "other", AmountCents: 10}); err != nil {
		t.Fatalf("unrelated Post: %v", err)
	}
	// Reads are unaffected.
	if b := l.Balance("sleeper"); b != 100 {
		t.Fatalf("dormant balance = %d, want 100", b)
	}
}

func TestDormantReplayBeforeMarking(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	orig, _, err := l.Post(JournalEntry{ID: "r1", DebitAccount: "sleeper", CreditAccount: "cp", AmountCents: 50, IdempotencyKey: "key-r1", CreatedAt: old})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	// A replay of the pre-dormancy key returns the original: the replay
	// books nothing new, mirroring the closed/frozen gate order.
	replay, dup, err := l.Post(JournalEntry{ID: "r1-dup", DebitAccount: "sleeper", CreditAccount: "cp", AmountCents: 50, IdempotencyKey: "key-r1"})
	if err != nil || !dup {
		t.Fatalf("replay = dup=%v err=%v, want dup=true", dup, err)
	}
	if replay.ID != orig.ID {
		t.Fatalf("replay ID = %q, want %q", replay.ID, orig.ID)
	}
}

func TestDormantRejectsTransfer(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 10000, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	// Dormant payer.
	_, err := l.PostTransfer(Transfer{ID: "t1", From: "sleeper", To: "fresh", AmountCents: 100, Currency: "USD"})
	if !errors.Is(err, ErrAccountDormant) {
		t.Fatalf("dormant payer err = %v, want ErrAccountDormant", err)
	}
	// Dormant payee.
	_, err = l.PostTransfer(Transfer{ID: "t2", From: "fresh", To: "sleeper", AmountCents: 100, Currency: "USD"})
	if !errors.Is(err, ErrAccountDormant) {
		t.Fatalf("dormant payee err = %v, want ErrAccountDormant", err)
	}
	// Unrelated transfer still books.
	if _, err := l.PostTransfer(Transfer{ID: "t3", From: "fresh", To: "other", AmountCents: 100, Currency: "USD"}); err != nil {
		t.Fatalf("unrelated transfer: %v", err)
	}
}

func TestDormantGateOrderVsFrozen(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	l.Freeze("sleeper")
	// Dormant is a lifecycle stop and precedes the frozen risk-control
	// stop in the gate order, like the closed check does.
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "sleeper", CreditAccount: "x", AmountCents: 10}); !errors.Is(err, ErrAccountDormant) {
		t.Fatalf("dormant+frozen err = %v, want ErrAccountDormant", err)
	}
	l.Unfreeze("sleeper")
}

func TestReactivateAccount(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	rec, was, err := l.ReactivateAccount("sleeper")
	if err != nil || !was {
		t.Fatalf("ReactivateAccount = was=%v err=%v, want was=true", was, err)
	}
	if rec.Account != "sleeper" || rec.LastActivity.IsZero() {
		t.Fatalf("cleared record = %+v, want account+timestamps", rec)
	}
	if l.IsDormant("sleeper") {
		t.Fatal("still dormant after reactivation")
	}
	// Postings flow again.
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "sleeper", CreditAccount: "x", AmountCents: 10}); err != nil {
		t.Fatalf("Post after reactivation: %v", err)
	}
	// Reactivating a non-dormant account is a no-op.
	if _, was, err := l.ReactivateAccount("sleeper"); err != nil || was {
		t.Fatalf("second reactivate = was=%v err=%v, want was=false", was, err)
	}
	if _, _, err := l.ReactivateAccount(""); !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("empty account err = %v, want ErrEmptyAccountID", err)
	}
}

func TestDormantAuditEvents(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	if _, _, err := l.ReactivateAccount("sleeper"); err != nil {
		t.Fatalf("ReactivateAccount: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	byOp := map[string]int{}
	for _, ev := range events {
		byOp[ev.Op]++
	}
	// The seed entry touched two accounts (sleeper, cp), both inactive:
	// each marking is audited.
	if byOp["dormant_marked"] != 2 {
		t.Fatalf("dormant_marked events = %d, want 2", byOp["dormant_marked"])
	}
	if byOp["dormant_reactivated"] != 1 {
		t.Fatalf("dormant_reactivated events = %d, want 1", byOp["dormant_reactivated"])
	}
}

func TestChargeDormancyFee(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100000, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	month := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fee, dup, err := l.ChargeDormancyFee("sleeper", "fee-income", 500, month, 0)
	if err != nil || dup {
		t.Fatalf("ChargeDormancyFee = dup=%v err=%v", dup, err)
	}
	// The fee charges the dormant account: the dormant gate is skipped
	// for the fee by design.
	if fee.ID != "dormant-fee/sleeper/fee-income/2026-10" {
		t.Fatalf("fee ID = %q, want deterministic monthly ID", fee.ID)
	}
	if fee.DebitAccount != "sleeper" || fee.CreditAccount != "fee-income" || fee.AmountCents != 500 {
		t.Fatalf("fee legs = %+v", fee)
	}
	if fee.Memo != "dormant-fee" {
		t.Fatalf("fee memo = %q, want dormant-fee", fee.Memo)
	}
	if b := l.Balance("sleeper"); b != 100000+500 {
		t.Fatalf("sleeper balance = %d, want 100500", b)
	}
	// Same month re-charges are idempotent.
	if _, dup, err := l.ChargeDormancyFee("sleeper", "fee-income", 500, month, 0); err != nil || !dup {
		t.Fatalf("re-charge = dup=%v err=%v, want dup=true", dup, err)
	}
	// A new month gets a fresh ID.
	if _, dup, err := l.ChargeDormancyFee("sleeper", "fee-income", 500, month.AddDate(0, 1, 0), 0); err != nil || dup {
		t.Fatalf("next month = dup=%v err=%v", dup, err)
	}
	total, err := l.DormancyFeeTotal("sleeper", "fee-income", time.Time{}, month.AddDate(0, 2, 0))
	if err != nil || total != 1000 {
		t.Fatalf("DormancyFeeTotal = %d, %v; want 1000", total, err)
	}
}

func TestChargeDormancyFeeCap(t *testing.T) {
	l := New()
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100000, time.Now().UTC().AddDate(0, 0, -60))
	month := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, _, err := l.ChargeDormancyFee("sleeper", "fee-income", 600, month, 1000); err != nil {
		t.Fatalf("first fee: %v", err)
	}
	// 600 + 500 > 1000 cap: rejected, nothing booked.
	if _, _, err := l.ChargeDormancyFee("sleeper", "fee-income", 500, month.AddDate(0, 1, 0), 1000); !errors.Is(err, ErrDormancyFeeCapExceeded) {
		t.Fatalf("over-cap err = %v, want ErrDormancyFeeCapExceeded", err)
	}
	if _, _, err := l.ChargeDormancyFee("sleeper", "fee-income", 1, month.AddDate(0, 2, 0), -1); !errors.Is(err, ErrInvalidDormancyFeeCap) {
		t.Fatalf("negative cap err = %v, want ErrInvalidDormancyFeeCap", err)
	}
	if _, _, err := l.ChargeDormancyFee("sleeper", "fee-income", 0, month, 0); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("zero amount err = %v, want ErrNonPositiveAmount", err)
	}
}

func TestDormancyFeeReversible(t *testing.T) {
	l := New()
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100000, time.Now().UTC().AddDate(0, 0, -60))
	month := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fee, _, err := l.ChargeDormancyFee("sleeper", "fee-income", 500, month, 0)
	if err != nil {
		t.Fatalf("ChargeDormancyFee: %v", err)
	}
	// The generic reversal path covers dormancy fees: no special-cased
	// reversal is needed.
	if _, _, err := l.ReverseEntry(fee.ID, "rev-key"); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	total, err := l.DormancyFeeTotal("sleeper", "fee-income", time.Time{}, time.Now())
	if err != nil || total != 0 {
		t.Fatalf("total after reversal = %d, %v; want 0", total, err)
	}
}

func TestDormantSnapshotRoundTrip(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100, old)
	report, err := l.DetectDormant(30)
	if err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	var sb strings.Builder
	if err := l.ExportSnapshot(&sb); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !l2.IsDormant("sleeper") {
		t.Fatal("dormancy mark lost across snapshot round-trip")
	}
	// The record is restored verbatim, not re-marked.
	l2.mu.RLock()
	got := l2.dormantCopyLocked()
	l2.mu.RUnlock()
	if len(got) != len(report.Marked) || !got[0].MarkedAt.Equal(report.Marked[0].MarkedAt) {
		t.Fatalf("restored marks = %+v, want %+v", got, report.Marked)
	}
	// And the gate still applies after restore.
	if _, _, err := l2.Post(JournalEntry{ID: "p1", DebitAccount: "sleeper", CreditAccount: "x", AmountCents: 10}); !errors.Is(err, ErrAccountDormant) {
		t.Fatalf("post-restore err = %v, want ErrAccountDormant", err)
	}
}

func TestDormantConcurrent(t *testing.T) {
	l := New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "old-1", "sleeper", "cp", 100000, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	// Concurrent operator posts through the dormant account all fail;
	// system fees still book. Run under -race.
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		go func(i int) {
			_, _, err := l.Post(JournalEntry{ID: string(rune('a'+i)) + "-post", DebitAccount: "sleeper", CreditAccount: "x", AmountCents: 10})
			if !errors.Is(err, ErrAccountDormant) {
				errs <- err
				return
			}
			errs <- nil
		}(i)
		go func(i int) {
			month := time.Date(2026, time.Month(1+i), 1, 12, 0, 0, 0, time.UTC)
			_, _, err := l.ChargeDormancyFee("sleeper", "fee-income", 10, month, 0)
			errs <- err
		}(i)
	}
	for i := 0; i < 16; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent: %v", err)
		}
	}
}

func TestDeclareDividendOnDormantHolder(t *testing.T) {
	l := New()
	// A dormant holder still receives its declared dividend: paying it
	// is the corporation's action, not operator activity through the
	// account — dividend legs post through the system path that skips
	// the dormant-account stop.
	old := time.Now().UTC().AddDate(0, 0, -60)
	postDormantSeed(t, l, "seed", "pool", "holder-a", 1, old)
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	if !l.IsDormant("holder-a") {
		t.Fatal("holder-a should be dormant")
	}
	holders := []DividendHolder{
		{Account: "holder-a", SharesBps: 6000},
		{Account: "holder-b", SharesBps: 4000},
	}
	r, err := l.DeclareDividend("pool", 10000, "USD", holders, time.Now().UTC())
	if err != nil {
		t.Fatalf("dividend to dormant holder: %v", err)
	}
	if len(r.Payouts) != 2 {
		t.Fatalf("payouts = %d, want 2", len(r.Payouts))
	}
}
