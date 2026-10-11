package ledger

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// nettingSeedSeq disambiguates journal entry IDs across
// seedNettingObligation calls within one test binary run.
var nettingSeedSeq atomic.Int64

// seedNettingObligation books a bilateral obligation onto the books:
// debtor ends at -cents, creditor at +cents, in the given currency. It
// is the test-visible way to create the net positions a netting session
// settles: debit the creditor (balance up), credit the debtor (balance
// down), mirroring how an accrual books a payable.
func seedNettingObligation(t *testing.T, l *Ledger, debtor, creditor AccountID, cents int64, currency string) {
	t.Helper()
	seq := nettingSeedSeq.Add(1)
	_, _, err := l.Post(JournalEntry{
		ID:            fmt.Sprintf("netting-obligation-%s-%d", t.Name(), seq),
		DebitAccount:  creditor,
		CreditAccount: debtor,
		AmountCents:   cents,
		Currency:      currency,
	})
	if err != nil {
		t.Fatalf("seedNettingObligation(%s owes %s %d %s): %v", debtor, creditor, cents, currency, err)
	}
}

func netPositionsMap(ps []NetPosition) map[NetPosition]int64 {
	out := make(map[NetPosition]int64, len(ps))
	for _, p := range ps {
		out[NetPosition{Account: p.Account, Currency: p.Currency}] = p.Cents
	}
	return out
}

func TestNetPositionsMultiCurrency(t *testing.T) {
	l := New()
	// USD book: a -10000, b +6000, c +4000 (sums to zero).
	seedNettingObligation(t, l, "a", "b", 6000, "USD")
	seedNettingObligation(t, l, "a", "c", 4000, "USD")
	// EUR book: a +5000, b -5000 (sums to zero) — isolated from USD.
	seedNettingObligation(t, l, "b", "a", 5000, "EUR")

	got, err := l.NetPositions([]AccountID{"a", "b", "c", "nobody"})
	if err != nil {
		t.Fatalf("NetPositions: %v", err)
	}
	want := map[NetPosition]int64{
		{Account: "a", Currency: "EUR"}: 5000,
		{Account: "b", Currency: "EUR"}: -5000,
		{Account: "a", Currency: "USD"}: -10000,
		{Account: "b", Currency: "USD"}: 6000,
		{Account: "c", Currency: "USD"}: 4000,
	}
	if m := netPositionsMap(got); fmt.Sprintf("%v", m) != fmt.Sprintf("%v", want) {
		t.Errorf("NetPositions = %v, want %v", m, want)
	}
	// Sorted by (currency, account): EUR rows before USD rows.
	if len(got) != 5 || got[0].Currency != "EUR" || got[0].Account != "a" || got[4].Currency != "USD" || got[4].Account != "c" {
		t.Errorf("NetPositions not sorted by (currency, account): %+v", got)
	}
	// "nobody" has no balance and contributes no row; zero positions are
	// omitted everywhere.
	for _, p := range got {
		if p.Account == "nobody" || p.Cents == 0 {
			t.Errorf("NetPositions returned a spurious row: %+v", p)
		}
	}
}

func TestNettingPlanBoundAndConservation(t *testing.T) {
	l := New()
	// Five accounts, one currency, positions summing to zero: the greedy
	// plan must settle in at most N-1 = 4 transfers.
	seedNettingObligation(t, l, "a", "b", 3000, "USD")
	seedNettingObligation(t, l, "a", "c", 4000, "USD")
	seedNettingObligation(t, l, "a", "d", 2000, "USD")
	seedNettingObligation(t, l, "a", "e", 1000, "USD")
	accounts := []AccountID{"a", "b", "c", "d", "e"}

	plan, err := l.PlanNetting("plan-1", accounts)
	if err != nil {
		t.Fatalf("PlanNetting: %v", err)
	}
	if len(plan.Transfers) > len(accounts)-1 {
		t.Errorf("plan has %d transfers, want <= %d (N-1)", len(plan.Transfers), len(accounts)-1)
	}
	// Conservation: every creditor's claim is paid exactly once.
	var total int64
	paid := make(map[AccountID]int64)
	for _, leg := range plan.Transfers {
		total += leg.AmountCents
		paid[leg.To] += leg.AmountCents
		if leg.From != "a" {
			t.Errorf("leg %+v: only the debtor a should pay", leg)
		}
		if leg.Currency != "USD" {
			t.Errorf("leg %+v: wrong currency", leg)
		}
	}
	if total != 10000 {
		t.Errorf("total settled = %d, want 10000 (sum of creditor claims)", total)
	}
	for _, tc := range []struct {
		account AccountID
		claim   int64
	}{{"b", 3000}, {"c", 4000}, {"d", 2000}, {"e", 1000}} {
		if paid[tc.account] != tc.claim {
			t.Errorf("creditor %s paid %d, want its full claim %d", tc.account, paid[tc.account], tc.claim)
		}
	}
	// The simulated after-positions are all zero: complete settlement.
	if len(plan.After) != 0 {
		t.Errorf("plan.After = %+v, want empty (fully settled)", plan.After)
	}
	// Determinism: the same positions always produce the same plan.
	again, err := l.PlanNetting("plan-1", accounts)
	if err != nil {
		t.Fatalf("PlanNetting again: %v", err)
	}
	if fmt.Sprintf("%+v", again.Transfers) != fmt.Sprintf("%+v", plan.Transfers) {
		t.Errorf("plan not deterministic:\n%+v\nvs\n%+v", plan.Transfers, again.Transfers)
	}
	// Planning books nothing.
	if v := l.Version(); v != 4 {
		t.Errorf("PlanNetting bumped version to %d, want 4 (seed posts only)", v)
	}
}

func TestNettingPlanCurrencyIsolation(t *testing.T) {
	l := New()
	// Two currencies with different participant shapes: USD settles fully,
	// EUR leaves a residual (b's EUR debt exceeds a's EUR claim).
	seedNettingObligation(t, l, "a", "b", 1000, "USD")
	seedNettingObligation(t, l, "a", "c", 1000, "EUR")

	plan, err := l.PlanNetting("plan-fx", []AccountID{"a", "b", "c"})
	if err != nil {
		t.Fatalf("PlanNetting: %v", err)
	}
	if len(plan.Transfers) != 2 {
		t.Fatalf("plan has %d transfers, want 2 (one per currency)", len(plan.Transfers))
	}
	for _, leg := range plan.Transfers {
		before := netPositionsMap(plan.Before)
		fromKey := NetPosition{Account: leg.From, Currency: leg.Currency}
		toKey := NetPosition{Account: leg.To, Currency: leg.Currency}
		if before[fromKey] >= 0 || before[toKey] <= 0 {
			t.Errorf("leg %+v crosses currency books: from=%d to=%d", leg, before[fromKey], before[toKey])
		}
		if leg.AmountCents <= 0 {
			t.Errorf("leg %+v has non-positive amount", leg)
		}
	}
	// EUR: b owes a 1000, a's claim settles in full. USD: a owes b 1000,
	// b's claim settles in full. Both currencies settle completely and
	// independently.
	if len(plan.After) != 0 {
		t.Errorf("plan.After = %+v, want empty", plan.After)
	}
}

func TestPostNettingHappyPath(t *testing.T) {
	l := New()
	seedNettingObligation(t, l, "a", "b", 3000, "USD")
	seedNettingObligation(t, l, "a", "c", 4000, "USD")
	seedNettingObligation(t, l, "a", "d", 2000, "USD")
	seedNettingObligation(t, l, "a", "e", 1000, "USD")
	accounts := []AccountID{"a", "b", "c", "d", "e"}
	_, versionBefore := l.Snapshot("nobody")

	report, err := l.PostNetting("settle-1", accounts)
	if err != nil {
		t.Fatalf("PostNetting: %v", err)
	}
	if report.Duplicate {
		t.Error("fresh PostNetting Duplicate = true, want false")
	}
	if len(report.Transfers) == 0 || len(report.Transfers) > len(accounts)-1 {
		t.Errorf("report has %d transfers, want 1..%d", len(report.Transfers), len(accounts)-1)
	}
	if len(report.EntryIDs) != len(report.Transfers) {
		t.Fatalf("EntryIDs (%d) != Transfers (%d)", len(report.EntryIDs), len(report.Transfers))
	}
	// Every leg is a real journal entry with the right legs and the
	// deterministic entry ID.
	for i, leg := range report.Transfers {
		if leg.EntryID != report.EntryIDs[i] {
			t.Errorf("leg %d EntryID %q != EntryIDs[%d] %q", i, leg.EntryID, i, report.EntryIDs[i])
		}
		e, ok := l.entries[leg.EntryID]
		if !ok {
			t.Errorf("journal entry %q missing", leg.EntryID)
			continue
		}
		if e.DebitAccount != leg.From || e.CreditAccount != leg.To ||
			e.AmountCents != leg.AmountCents || e.Currency != leg.Currency {
			t.Errorf("journal entry %q = %+v, want debit=%s credit=%s %d %s",
				leg.EntryID, e, leg.From, leg.To, leg.AmountCents, leg.Currency)
		}
		if e.BatchID != "settle-1" {
			t.Errorf("journal entry %q BatchID = %q, want the session ID", leg.EntryID, e.BatchID)
		}
	}
	// Report contents: before positions, per-leg detail, after positions.
	if m := netPositionsMap(report.Before); m[NetPosition{Account: "a", Currency: "USD"}] != -10000 {
		t.Errorf("report.Before missing a's USD debt: %+v", report.Before)
	}
	if len(report.After) != 0 {
		t.Errorf("report.After = %+v, want empty (fully settled)", report.After)
	}
	for _, a := range accounts {
		if got := l.BalanceIn(a, "USD"); got != 0 {
			t.Errorf("BalanceIn(%s, USD) = %d after settlement, want 0", a, got)
		}
	}
	if _, v := l.Snapshot("nobody"); v != versionBefore+uint64(len(report.Transfers)) {
		t.Errorf("version = %d, want %d + %d legs", v, versionBefore, len(report.Transfers))
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation after netting: %v", err)
	}
}

func TestPostNettingAtomicFrozen(t *testing.T) {
	l := New()
	seedNettingObligation(t, l, "d", "c1", 100, "USD")
	seedNettingObligation(t, l, "d", "c2", 200, "USD")
	// c2 is frozen: the d->c2 leg must fail, and the d->c1 leg — which
	// would pass on its own — must not book either.
	l.Freeze("c2")
	_, versionBefore := l.Snapshot("nobody")
	entriesBefore := len(l.Entries())

	_, err := l.PostNetting("settle-frozen", []AccountID{"d", "c1", "c2"})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("PostNetting err = %v, want ErrAccountFrozen", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
	for _, tc := range []struct {
		account AccountID
		want    int64
	}{{"d", -300}, {"c1", 100}, {"c2", 200}} {
		if got := l.BalanceIn(tc.account, "USD"); got != tc.want {
			t.Errorf("BalanceIn(%s) = %d, want %d (atomic: nothing booked)", tc.account, got, tc.want)
		}
	}
	// No session key was registered for the failed session: unfreezing
	// and reposting the same session settles normally.
	l.Unfreeze("c2")
	report, err := l.PostNetting("settle-frozen", []AccountID{"d", "c1", "c2"})
	if err != nil {
		t.Fatalf("PostNetting after unfreeze: %v", err)
	}
	if report.Duplicate || len(report.Transfers) != 2 {
		t.Errorf("repost after unfreeze: duplicate=%v legs=%d, want false/2", report.Duplicate, len(report.Transfers))
	}
}

func TestPostNettingAtomicDailyLimit(t *testing.T) {
	l := New()
	seedNettingObligation(t, l, "d", "c", 1000, "USD")
	// The daily outflow limit guards the credit (payee-side) leg of each
	// settlement entry; a 1000-cent leg against a 500-cent limit fails.
	if err := l.SetDailyLimit("c", "USD", 500); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}
	_, versionBefore := l.Snapshot("nobody")
	entriesBefore := len(l.Entries())

	_, err := l.PostNetting("settle-limit", []AccountID{"d", "c"})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("PostNetting err = %v, want ErrDailyLimitExceeded", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
	if got := l.BalanceIn("d", "USD"); got != -1000 {
		t.Errorf("BalanceIn(d) = %d, want -1000 (atomic: nothing booked)", got)
	}
}

func TestPostNettingIdempotentReplay(t *testing.T) {
	l := New()
	seedNettingObligation(t, l, "a", "b", 1500, "USD")
	seedNettingObligation(t, l, "a", "c", 500, "USD")
	accounts := []AccountID{"a", "b", "c"}

	first, err := l.PostNetting("settle-replay", accounts)
	if err != nil {
		t.Fatalf("PostNetting: %v", err)
	}
	_, versionBefore := l.Snapshot("nobody")
	entriesBefore := len(l.Entries())

	second, err := l.PostNetting("settle-replay", accounts)
	if err != nil {
		t.Fatalf("PostNetting replay: %v", err)
	}
	if !second.Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
	if fmt.Sprintf("%+v", second.Transfers) != fmt.Sprintf("%+v", first.Transfers) ||
		fmt.Sprintf("%+v", second.Before) != fmt.Sprintf("%+v", first.Before) ||
		fmt.Sprintf("%+v", second.After) != fmt.Sprintf("%+v", first.After) ||
		fmt.Sprintf("%v", second.EntryIDs) != fmt.Sprintf("%v", first.EntryIDs) {
		t.Errorf("replay report differs from the original:\nfirst=%+v\nsecond=%+v", first, second)
	}
	// The replay books nothing new, so it must not fail on state that
	// changed after the original settlement — freezing an account now
	// still returns the original receipt.
	l.Freeze("b")
	third, err := l.PostNetting("settle-replay", accounts)
	if err != nil {
		t.Fatalf("PostNetting replay after freeze: %v", err)
	}
	if !third.Duplicate || fmt.Sprintf("%+v", third.Transfers) != fmt.Sprintf("%+v", first.Transfers) {
		t.Errorf("replay after freeze did not return the original receipt: %+v", third)
	}
}

func TestPostNettingResidual(t *testing.T) {
	l := New()
	// The set does not sum to zero: b's 600 claim settles, d keeps a -400
	// residual owed to parties outside the netting set.
	seedNettingObligation(t, l, "d", "b", 600, "USD")
	// d also owes 400 outside the set: model it as a debt to an
	// outsider account that is not part of the session.
	seedNettingObligation(t, l, "d", "outsider", 400, "USD")

	report, err := l.PostNetting("settle-residual", []AccountID{"d", "b"})
	if err != nil {
		t.Fatalf("PostNetting: %v", err)
	}
	if len(report.Transfers) != 1 {
		t.Fatalf("report has %d transfers, want 1", len(report.Transfers))
	}
	leg := report.Transfers[0]
	if leg.From != "d" || leg.To != "b" || leg.AmountCents != 600 || leg.Currency != "USD" {
		t.Errorf("leg = %+v, want d->b 600 USD", leg)
	}
	// <= N-1 legs even with a residual: N=2, one leg.
	if m := netPositionsMap(report.After); len(m) != 1 || m[NetPosition{Account: "d", Currency: "USD"}] != -400 {
		t.Errorf("report.After = %+v, want only d at -400 USD (residual)", report.After)
	}
	if got := l.BalanceIn("b", "USD"); got != 0 {
		t.Errorf("BalanceIn(b) = %d, want 0 (fully settled)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}

func TestPostNettingEmptyPlan(t *testing.T) {
	l := New()
	_, versionBefore := l.Snapshot("nobody")

	report, err := l.PostNetting("settle-empty", []AccountID{"a", "b"})
	if err != nil {
		t.Fatalf("PostNetting with no obligations: %v", err)
	}
	if report.Duplicate || len(report.Transfers) != 0 || len(report.EntryIDs) != 0 {
		t.Errorf("empty report = %+v, want no transfers and Duplicate=false", report)
	}
	if _, v := l.Snapshot("nobody"); v != versionBefore {
		t.Errorf("empty netting bumped version to %d, want %d", v, versionBefore)
	}
	// The empty session is still registered: its replay is stable.
	again, err := l.PostNetting("settle-empty", []AccountID{"a", "b"})
	if err != nil {
		t.Fatalf("PostNetting empty replay: %v", err)
	}
	if !again.Duplicate || len(again.Transfers) != 0 {
		t.Errorf("empty replay = %+v, want Duplicate=true with no transfers", again)
	}
}

func TestPostNettingValidation(t *testing.T) {
	l := New()
	if _, err := l.PostNetting("", []AccountID{"a"}); !errors.Is(err, ErrEmptyNettingSession) {
		t.Errorf("empty session err = %v, want ErrEmptyNettingSession", err)
	}
	if _, err := l.PostNetting("s", nil); !errors.Is(err, ErrEmptyNettingAccounts) {
		t.Errorf("empty accounts err = %v, want ErrEmptyNettingAccounts", err)
	}
	if _, err := l.PostNetting("s", []AccountID{"a", "a"}); !errors.Is(err, ErrNettingDuplicateAccount) {
		t.Errorf("duplicate account err = %v, want ErrNettingDuplicateAccount", err)
	}
	if _, err := l.PostNetting("s", []AccountID{""}); !errors.Is(err, ErrEmptyNettingAccount) {
		t.Errorf("empty account err = %v, want ErrEmptyNettingAccount", err)
	}
	if _, err := l.PlanNetting("", []AccountID{"a"}); !errors.Is(err, ErrEmptyNettingSession) {
		t.Errorf("PlanNetting empty session err = %v, want ErrEmptyNettingSession", err)
	}
	if _, err := l.NetPositions(nil); !errors.Is(err, ErrEmptyNettingAccounts) {
		t.Errorf("NetPositions nil err = %v, want ErrEmptyNettingAccounts", err)
	}
}

// TestNettingBoundAcrossShapes exercises the <= N-1 bound on several
// position shapes, including a many-debtor shape and a residual shape.
func TestNettingBoundAcrossShapes(t *testing.T) {
	shapes := []struct {
		name      string
		positions []NetPosition // single currency; may not sum to zero
		n         int
	}{
		{"one-debtor-five-creditors", []NetPosition{
			{Account: "d", Currency: "USD", Cents: -1000},
			{Account: "c1", Currency: "USD", Cents: 100},
			{Account: "c2", Currency: "USD", Cents: 200},
			{Account: "c3", Currency: "USD", Cents: 300},
			{Account: "c4", Currency: "USD", Cents: 400},
		}, 5},
		{"five-debtors-one-creditor", []NetPosition{
			{Account: "d1", Currency: "USD", Cents: -100},
			{Account: "d2", Currency: "USD", Cents: -200},
			{Account: "d3", Currency: "USD", Cents: -300},
			{Account: "d4", Currency: "USD", Cents: -400},
			{Account: "c", Currency: "USD", Cents: 1000},
		}, 5},
		{"residual-shape", []NetPosition{
			{Account: "d1", Currency: "USD", Cents: -700},
			{Account: "d2", Currency: "USD", Cents: -500},
			{Account: "c1", Currency: "USD", Cents: 900},
		}, 3},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			legs := greedyNettingLegs("bound-test", sh.positions)
			if len(legs) > sh.n-1 {
				t.Errorf("%d legs, want <= %d (N-1)", len(legs), sh.n-1)
			}
			// Conservation: the total paid out equals the total creditor
			// claims settled. In every shape above the creditors settle
			// in full (claims <= debts), so it equals the sum of
			// positive positions.
			var totalPaid, totalClaims int64
			for _, leg := range legs {
				totalPaid += leg.AmountCents
			}
			for _, p := range sh.positions {
				if p.Cents > 0 {
					totalClaims += p.Cents
				}
			}
			if totalPaid != totalClaims {
				t.Errorf("total paid = %d, want total creditor claims %d", totalPaid, totalClaims)
			}
			after := applyNettingLegs(sh.positions, legs)
			// After applying the legs, at most one participant keeps a
			// nonzero remainder (the residual); with zero-sum shapes,
			// none do.
			if len(after) > 1 {
				t.Errorf("after has %d nonzero rows, want <= 1: %+v", len(after), after)
			}
			var sumBefore, sumAfter int64
			for _, p := range sh.positions {
				sumBefore += p.Cents
			}
			for _, p := range after {
				sumAfter += p.Cents
			}
			if sumBefore != sumAfter {
				t.Errorf("position sum changed: before=%d after=%d (legs must conserve)", sumBefore, sumAfter)
			}
		})
	}
}
