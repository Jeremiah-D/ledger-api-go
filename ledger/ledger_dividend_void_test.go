package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Voiding a declared dividend (LG-360): the whole declaration reverses
// leg by leg through ReverseEntry, atomically (twin precheck, zero
// records on any rejection), idempotently, and visibly — Dividends()
// and Reconcile carry the void marker, and re-declaring a voided
// dividend replays the original receipt instead of resurrecting it.

func mustDeclareDividend(t *testing.T, l *Ledger, pool AccountID, total int64, currency string) DividendReceipt {
	t.Helper()
	r, err := l.DeclareDividend(pool, total, currency, dividendHolders(), dividendDay)
	if err != nil {
		t.Fatalf("DeclareDividend: %v", err)
	}
	return r
}

func TestVoidDividendHappyPath(t *testing.T) {
	l := New()
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")

	vr, err := l.VoidDividend(r.DividendID, "operator-1")
	if err != nil {
		t.Fatalf("VoidDividend: %v", err)
	}
	if vr.Duplicate {
		t.Fatal("first void must not be a duplicate")
	}
	if vr.DividendID != r.DividendID || vr.Pool != "retained-earnings" ||
		vr.TotalCents != 10000 || vr.Currency != "USD" {
		t.Fatalf("void receipt = %+v, want the declared identity and totals", vr)
	}
	if !vr.RecordDate.Equal(dividendDay) {
		t.Fatalf("void record date = %v, want %v", vr.RecordDate, dividendDay)
	}
	if vr.VoidedAt.IsZero() {
		t.Fatal("void receipt VoidedAt must be set")
	}
	if len(vr.ReversedLegs) != 2 {
		t.Fatalf("reversed legs = %d, want 2", len(vr.ReversedLegs))
	}
	for i, id := range vr.ReversedLegs {
		want := r.Payouts[i].EntryID + "/reversal"
		if id != want {
			t.Fatalf("reversed leg %d = %q, want %q", i, id, want)
		}
	}

	// Both legs netted to zero: pool and holders are back where they
	// started, and the books still balance.
	if b := l.Balance("retained-earnings"); b != 0 {
		t.Fatalf("pool balance = %d, want 0", b)
	}
	if b := l.Balance("holder-a"); b != 0 {
		t.Fatalf("holder-a balance = %d, want 0", b)
	}
	if b := l.Balance("holder-b"); b != 0 {
		t.Fatalf("holder-b balance = %d, want 0", b)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
	// Two declaration legs plus two reversals, nothing else.
	if got := len(l.Entries()); got != 4 {
		t.Fatalf("entries = %d, want 4", got)
	}

	// Each reversal is a genuine ReverseEntry row: swapped legs, the
	// reversal memo, and the dividend-void marker in metadata.
	byID := make(map[string]JournalEntry, len(l.Entries()))
	for _, e := range l.Entries() {
		byID[e.ID] = e
	}
	for i, p := range r.Payouts {
		rev, ok := byID[reversalIDFor(p.EntryID)]
		if !ok {
			t.Fatalf("reversal of leg %d missing", i)
		}
		if rev.Memo != "reversal_of:"+p.EntryID {
			t.Fatalf("reversal memo = %q, want reversal_of:<leg>", rev.Memo)
		}
		if rev.Metadata[dividendVoidMetadataKey] != "dividend-void:"+r.DividendID {
			t.Fatalf("reversal metadata = %v, want the dividend-void marker", rev.Metadata)
		}
		if rev.DebitAccount != p.Account || rev.CreditAccount != "retained-earnings" {
			t.Fatalf("reversal legs = %q/%q, want holder/pool", rev.DebitAccount, rev.CreditAccount)
		}
		if rev.AmountCents != p.AmountCents || rev.Currency != "USD" {
			t.Fatalf("reversal amount = %d %s, want %d USD", rev.AmountCents, rev.Currency, p.AmountCents)
		}
	}
}

func TestVoidDividendIdempotent(t *testing.T) {
	l := New()
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")
	first, err := l.VoidDividend(r.DividendID, "operator-1")
	if err != nil {
		t.Fatalf("first VoidDividend: %v", err)
	}
	entriesAfterFirst := len(l.Entries())
	versionAfterFirst := l.Version()

	second, err := l.VoidDividend(r.DividendID, "operator-1")
	if err != nil {
		t.Fatalf("second VoidDividend: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("re-void must return Duplicate == true")
	}
	if second.DividendID != first.DividendID || !second.VoidedAt.Equal(first.VoidedAt) {
		t.Fatalf("re-void receipt = %+v, want the original receipt", second)
	}
	if len(second.ReversedLegs) != len(first.ReversedLegs) {
		t.Fatalf("re-void reversed legs = %v, want %v", second.ReversedLegs, first.ReversedLegs)
	}
	if got := len(l.Entries()); got != entriesAfterFirst {
		t.Fatalf("re-void journaled %d entries, want %d (zero new rows)", got, entriesAfterFirst)
	}
	if got := l.Version(); got != versionAfterFirst {
		t.Fatalf("re-void version = %d, want %d (no bump)", got, versionAfterFirst)
	}
}

func TestVoidDividendNotFound(t *testing.T) {
	l := New()
	if _, err := l.VoidDividend("dividend/nope/2026-10-15", "operator-1"); !errors.Is(err, ErrDividendNotFound) {
		t.Fatalf("VoidDividend unknown ID: err = %v, want ErrDividendNotFound", err)
	}
}

func TestVoidDividendFrozenLegZeroRecords(t *testing.T) {
	l := New()
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")
	entriesBefore := len(l.Entries())
	versionBefore := l.Version()

	// Freeze the second holder: its leg's reversal is rejected, so
	// the whole void must abort with zero records — not even the
	// first leg may land.
	l.Freeze("holder-b")
	if _, err := l.VoidDividend(r.DividendID, "operator-1"); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("VoidDividend frozen leg: err = %v, want ErrAccountFrozen", err)
	}
	if got := len(l.Entries()); got != entriesBefore {
		t.Fatalf("failed void journaled %d entries, want %d (zero records)", got, entriesBefore)
	}
	if got := l.Version(); got != versionBefore {
		t.Fatalf("failed void version = %d, want %d (no bump)", got, versionBefore)
	}
	// The declaration is untouched and still live.
	divs := l.Dividends()
	if len(divs) != 1 || divs[0].VoidedAt != nil {
		t.Fatalf("Dividends after failed void = %+v, want one live dividend", divs)
	}

	// Unfreeze and void: the same declaration voids cleanly.
	l.Unfreeze("holder-b")
	if _, err := l.VoidDividend(r.DividendID, "operator-1"); err != nil {
		t.Fatalf("VoidDividend after unfreeze: %v", err)
	}
}

func TestVoidDividendDividendsShowsVoided(t *testing.T) {
	l := New()
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")

	divs := l.Dividends()
	if len(divs) != 1 || divs[0].VoidedAt != nil {
		t.Fatalf("Dividends before void = %+v, want one live dividend", divs)
	}
	if _, err := l.VoidDividend(r.DividendID, "operator-1"); err != nil {
		t.Fatalf("VoidDividend: %v", err)
	}
	divs = l.Dividends()
	if len(divs) != 1 {
		t.Fatalf("Dividends after void = %d, want 1", len(divs))
	}
	if divs[0].VoidedAt == nil {
		t.Fatal("Dividends after void: VoidedAt must be set")
	}
	// The declaration record survives the void: same ID, totals, and
	// payouts — only the void marker is new.
	if divs[0].DividendID != r.DividendID || divs[0].TotalCents != 10000 || len(divs[0].Payouts) != 2 {
		t.Fatalf("voided Dividends receipt = %+v, want the original declaration plus the marker", divs[0])
	}
}

func TestVoidDividendReconcileDiscloses(t *testing.T) {
	l := New()
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")
	if _, err := l.VoidDividend(r.DividendID, "operator-1"); err != nil {
		t.Fatalf("VoidDividend: %v", err)
	}
	rep := l.Reconcile(time.Now())
	if len(rep.Dividends) != 1 {
		t.Fatalf("Reconcile dividends = %d, want 1", len(rep.Dividends))
	}
	if rep.Dividends[0].VoidedAt == nil {
		t.Fatal("Reconcile must disclose the voided dividend's VoidedAt")
	}
	if rep.Dividends[0].DividendID != r.DividendID {
		t.Fatalf("Reconcile dividend = %q, want %q", rep.Dividends[0].DividendID, r.DividendID)
	}
}

func TestVoidDividendRedeclareReturnsOriginal(t *testing.T) {
	l := New()
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")
	if _, err := l.VoidDividend(r.DividendID, "operator-1"); err != nil {
		t.Fatalf("VoidDividend: %v", err)
	}
	entriesAfterVoid := len(l.Entries())

	// Re-declaring the same (pool, recordDate) replays the original
	// receipt — the void is not undone and nothing is re-booked.
	again, err := l.DeclareDividend("retained-earnings", 10000, "USD", dividendHolders(), dividendDay)
	if err != nil {
		t.Fatalf("re-declare after void: %v", err)
	}
	if !again.Duplicate {
		t.Fatal("re-declare after void must be a duplicate replay")
	}
	if again.VoidedAt == nil {
		t.Fatal("re-declared receipt must keep the void marker")
	}
	if len(again.Payouts) != 2 || again.Payouts[0].AmountCents != 6000 {
		t.Fatalf("re-declared payouts = %+v, want the original legs", again.Payouts)
	}
	if got := len(l.Entries()); got != entriesAfterVoid {
		t.Fatalf("re-declare journaled %d entries, want %d (no resurrection)", got, entriesAfterVoid)
	}
	if b := l.Balance("retained-earnings"); b != 0 {
		t.Fatalf("pool balance after re-declare = %d, want 0", b)
	}
}

func TestVoidDividendSingleLeg(t *testing.T) {
	l := New()
	r, err := l.DeclareDividend("pool", 5000, "USD",
		[]DividendHolder{{Account: "sole", SharesBps: 10000}}, dividendDay)
	if err != nil {
		t.Fatalf("DeclareDividend: %v", err)
	}
	vr, err := l.VoidDividend(r.DividendID, "operator-1")
	if err != nil {
		t.Fatalf("VoidDividend: %v", err)
	}
	if len(vr.ReversedLegs) != 1 || vr.TotalCents != 5000 {
		t.Fatalf("single-leg void receipt = %+v, want one reversed leg of 5000", vr)
	}
	if b := l.Balance("pool"); b != 0 {
		t.Fatalf("pool balance = %d, want 0", b)
	}
	if b := l.Balance("sole"); b != 0 {
		t.Fatalf("holder balance = %d, want 0", b)
	}
}

func TestVoidDividendCrossCurrency(t *testing.T) {
	l := New()
	eur := mustDeclareDividend(t, l, "pool-eur", 5000, "EUR")
	usd := mustDeclareDividend(t, l, "pool-usd", 7000, "USD")

	if _, err := l.VoidDividend(eur.DividendID, "operator-1"); err != nil {
		t.Fatalf("VoidDividend EUR: %v", err)
	}
	// The EUR declaration nets to zero; the USD one is untouched.
	if b := l.Balance("pool-eur"); b != 0 {
		t.Fatalf("EUR pool balance = %d, want 0", b)
	}
	if b := l.Balance("pool-usd"); b != 7000 {
		t.Fatalf("USD pool balance = %d, want 7000 (untouched)", b)
	}
	divs := l.Dividends()
	if len(divs) != 2 {
		t.Fatalf("Dividends = %d, want 2", len(divs))
	}
	for _, d := range divs {
		switch d.DividendID {
		case eur.DividendID:
			if d.VoidedAt == nil {
				t.Fatal("EUR dividend must be marked voided")
			}
		case usd.DividendID:
			if d.VoidedAt != nil {
				t.Fatal("USD dividend must stay live")
			}
		default:
			t.Fatalf("unexpected dividend %q", d.DividendID)
		}
	}
}

func TestVoidDividendPartialManualReversal(t *testing.T) {
	l := New()
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")

	// Reverse one leg by hand first: VoidDividend must replay that
	// leg and reverse only the other, booking exactly one new row.
	if _, _, err := l.ReverseEntry(r.Payouts[0].EntryID, ""); err != nil {
		t.Fatalf("manual ReverseEntry: %v", err)
	}
	entriesBefore := len(l.Entries())
	vr, err := l.VoidDividend(r.DividendID, "operator-1")
	if err != nil {
		t.Fatalf("VoidDividend: %v", err)
	}
	if vr.Duplicate {
		t.Fatal("void with one fresh leg must not be a duplicate")
	}
	if len(vr.ReversedLegs) != 2 {
		t.Fatalf("reversed legs = %d, want 2", len(vr.ReversedLegs))
	}
	if got := len(l.Entries()); got != entriesBefore+1 {
		t.Fatalf("void journaled %d entries, want %d (one fresh reversal)", got, entriesBefore+1)
	}
	if b := l.Balance("retained-earnings"); b != 0 {
		t.Fatalf("pool balance = %d, want 0", b)
	}
}

func TestVoidDividendAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	r := mustDeclareDividend(t, l, "retained-earnings", 10000, "USD")
	if _, err := l.VoidDividend(r.DividendID, "operator-1"); err != nil {
		t.Fatalf("VoidDividend: %v", err)
	}

	var voids []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "dividend_voided" {
			voids = append(voids, e)
		}
	}
	if len(voids) != 1 {
		t.Fatalf("dividend_voided events = %d, want exactly 1", len(voids))
	}
	ev := voids[0]
	if ev.Actor != "operator-1" {
		t.Fatalf("audit actor = %q, want operator-1", ev.Actor)
	}
	if ev.TraceID != r.DividendID {
		t.Fatalf("audit trace ID = %q, want %q", ev.TraceID, r.DividendID)
	}
	if len(ev.EntryIDs) != 2 {
		t.Fatalf("audit entry IDs = %v, want the two reversal IDs", ev.EntryIDs)
	}
	for _, id := range ev.EntryIDs {
		if !strings.HasSuffix(id, "/reversal") {
			t.Fatalf("audit entry ID %q is not a reversal ID", id)
		}
	}
}
