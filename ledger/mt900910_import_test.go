package ledger

import (
	"errors"
	"testing"
	"time"
)

// mt900910Doc renders a minimal MT900/MT910 confirmation for the
// tests through the real export: :20: :21: :25: :32A:, no :52A:,
// no :72:.
func mt900910Doc(t *testing.T, msgType, ref, related, acct string, vd time.Time, cents int64) string {
	t.Helper()
	doc, err := ExportMT900910(MT900910{
		MessageType: msgType, SenderLT: "BANKUS33", ReceiverLT: "BANKDEFF",
		TransactionReference: ref, RelatedReference: related, Account: acct,
		ValueDate: vd, Currency: "USD", AmountCents: cents,
	})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// mt900910Seed posts one journal row for the tests.
func mt900910Seed(t *testing.T, l *Ledger, id, debit, credit, key string, cents int64, at time.Time, pending bool) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
		AmountCents: cents, Currency: "USD", IdempotencyKey: key,
		CreatedAt: at, PendingReview: pending,
	}); err != nil {
		t.Fatal(err)
	}
}

// The :21: cites the original MT103 :20: while the journal row
// carries "mt103-"+ref as its idempotency key: the prefix strip
// is what makes the confirmation hit. The :32A: value date is a
// day after the booking date and still matches — the reference
// path does not compare dates.
func TestImportMT900910PrefixStripMatch(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "ps-e1", "bank", "acct1", "mt103-REF123", 10000,
		time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-1", "REF123", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.MessageType != "900" || rep.TransactionReference != "CONF-1" || rep.RelatedReference != "REF123" {
		t.Errorf("header = %q/%q/%q, want 900/CONF-1/REF123", rep.MessageType, rep.TransactionReference, rep.RelatedReference)
	}
	if rep.Currency != "USD" || rep.AmountCents != 10000 {
		t.Errorf("amount = %s %d, want USD 10000", rep.Currency, rep.AmountCents)
	}
	if !rep.ValueDate.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("value date = %v, want 2026-10-05", rep.ValueDate)
	}
	if rep.Match.Status != CAMT053Matched || rep.Match.EntryID != "ps-e1" {
		t.Errorf("match = %+v, want matched ps-e1", rep.Match)
	}
	if rep.Match.EntryCredit {
		t.Errorf("entry credit = true, want false (MT900 needs the credit leg)")
	}
	if rep.Match.Reference != "REF123" || rep.Match.Supplementary != "" {
		t.Errorf("match reference/supplementary = %q/%q, want REF123/empty", rep.Match.Reference, rep.Match.Supplementary)
	}
	if rep.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
}

// The :21: cites a payout ID: the settle leg's idempotency key
// is "payout/<id>/settle", so the payout-derived key hits —
// for an MT900 against the credit leg and an MT910 against the
// debit leg.
func TestImportMT900910PayoutDerivedKey(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "payout/PO-7/settle", "bank", "acct1", "payout/PO-7/settle", 25000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	mt900910Seed(t, l, "payout/PO-8/settle", "acct1", "bank", "payout/PO-8/settle", 30000,
		time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC), false)

	rep, err := l.ImportMT900910("acct1", []byte(mt900910Doc(t, "900", "CONF-7", "PO-7", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 25000)))
	if err != nil {
		t.Fatalf("ImportMT900910 (MT900 payout): %v", err)
	}
	if rep.Match.Status != CAMT053Matched || rep.Match.EntryID != "payout/PO-7/settle" {
		t.Errorf("MT900 payout match = %+v, want matched payout/PO-7/settle", rep.Match)
	}

	rep, err = l.ImportMT900910("acct1", []byte(mt900910Doc(t, "910", "CONF-8", "PO-8", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 30000)))
	if err != nil {
		t.Fatalf("ImportMT900910 (MT910 payout): %v", err)
	}
	if rep.Match.Status != CAMT053Matched || rep.Match.EntryID != "payout/PO-8/settle" {
		t.Errorf("MT910 payout match = %+v, want matched payout/PO-8/settle", rep.Match)
	}
	if !rep.Match.EntryCredit {
		t.Errorf("MT910 entry credit = false, want true (MT910 needs the debit leg)")
	}
}

// A reference hit whose amount disagrees is mismatched, with
// both sides in the report.
func TestImportMT900910AmountMismatch(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "am-e1", "bank", "acct1", "mt103-AMT1", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-AM", "AMT1", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 12000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Mismatched {
		t.Fatalf("match = %+v, want mismatched", rep.Match)
	}
	if rep.Match.EntryID != "am-e1" || rep.Match.EntryAmountCents != 10000 || rep.Match.Reason == "" {
		t.Errorf("match = %+v, want entry am-e1 at 10000 with a reason", rep.Match)
	}
}

// A reference hit on the wrong leg is mismatched: an MT900 debit
// confirmation needs the account as the credit leg, and this row
// books it as the debit leg.
func TestImportMT900910DirectionMismatch(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "dm-e1", "acct1", "bank", "mt103-DIR1", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-DM", "DIR1", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Mismatched {
		t.Fatalf("match = %+v, want mismatched", rep.Match)
	}
	if rep.Match.EntryID != "dm-e1" || !rep.Match.EntryCredit || rep.Match.Reason == "" {
		t.Errorf("match = %+v, want entry dm-e1 with entry_credit true and a reason", rep.Match)
	}
}

// A :21: no journal row carries is unmatched, never an error.
func TestImportMT900910Unmatched(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "un-e1", "bank", "acct1", "mt103-OTHER", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-UN", "NOPE", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Unmatched || rep.Match.EntryID != "" || rep.Match.Reason == "" {
		t.Errorf("match = %+v, want unmatched with a reason and no entry", rep.Match)
	}
}

// Two rows whose references truncate to the same 16x key are
// ambiguous: mismatched with a candidate count.
func TestImportMT900910MultipleCandidates(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "shared-ref-00000001", "bank", "acct1", "", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	mt900910Seed(t, l, "shared-ref-00000002", "bank", "acct1", "", 10000,
		time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-MC", "shared-ref-00000", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Mismatched || rep.Match.CandidateCount != 2 {
		t.Errorf("match = %+v, want mismatched with candidate_count 2", rep.Match)
	}
}

// Pending-review rows never touched balances and are excluded:
// a confirmation citing one is unmatched, not matched.
func TestImportMT900910PendingReviewExcluded(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "pr-e1", "bank", "acct1", "mt103-PEND1", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), true)
	doc := mt900910Doc(t, "900", "CONF-PR", "PEND1", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Unmatched {
		t.Errorf("match = %+v, want unmatched (pending-review row excluded)", rep.Match)
	}
}

// NONREF fallback: with no reference to match, a single row in
// the :32A: currency, on the expected leg, whose effective value
// date falls on the :32A: day matches.
func TestImportMT900910NonRefSingleCandidate(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "nr-e1", "bank", "acct1", "unrelated-key", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-NR", "NONREF", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Matched || rep.Match.EntryID != "nr-e1" {
		t.Errorf("match = %+v, want matched nr-e1", rep.Match)
	}
}

// NONREF fallback with two same-day, same-currency, same-leg
// rows is ambiguous: mismatched with a candidate count, even
// though one row's amount agrees.
func TestImportMT900910NonRefAmbiguous(t *testing.T) {
	l := New()
	day := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	mt900910Seed(t, l, "na-e1", "bank", "acct1", "key-a", 10000, day, false)
	mt900910Seed(t, l, "na-e2", "bank", "acct1", "key-b", 20000, day.Add(time.Hour), false)
	doc := mt900910Doc(t, "900", "CONF-NA", "NONREF", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Mismatched || rep.Match.CandidateCount != 2 {
		t.Errorf("match = %+v, want mismatched with candidate_count 2", rep.Match)
	}
}

// NONREF fallback with no same-day row is unmatched — a row on
// another day does not count.
func TestImportMT900910NonRefNoCandidate(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "nn-e1", "bank", "acct1", "key-c", 10000,
		time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-NN", "NONREF", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.Match.Status != CAMT053Unmatched {
		t.Errorf("match = %+v, want unmatched", rep.Match)
	}
}

// A confirmation naming a different account than the import
// target is refused; a confirmation for an unknown account is
// refused with the unknown-account error. A :25: IBAN registered
// on the account resolves to it.
func TestImportMT900910WrongAndUnknownAccount(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "wa-e1", "bank", "acct1", "mt103-WA1", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-WA", "WA1", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	if _, err := l.ImportMT900910("acct2", []byte(doc)); err == nil {
		t.Errorf("wrong account: ImportMT900910 accepted another account's confirmation")
	} else if errors.Is(err, ErrMT900910UnknownAccount) {
		t.Errorf("wrong account: err = %v, want a non-unknown-account error", err)
	}

	ghost := mt900910Doc(t, "900", "CONF-GH", "GH1", "ghost-acct",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)
	if _, err := l.ImportMT900910("ghost-acct", []byte(ghost)); !errors.Is(err, ErrMT900910UnknownAccount) {
		t.Errorf("unknown account: err = %v, want ErrMT900910UnknownAccount", err)
	}

	iban := "DE89370400440532013000"
	if err := l.SetAccountIBAN("acct1", iban, "DEUTDEFF"); err != nil {
		t.Fatal(err)
	}
	ibanDoc := mt900910Doc(t, "900", "CONF-IB", "WA1", iban,
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)
	rep, err := l.ImportMT900910("acct1", []byte(ibanDoc))
	if err != nil {
		t.Fatalf("ImportMT900910 (:25: IBAN): %v", err)
	}
	if rep.Account != "acct1" || rep.Match.Status != CAMT053Matched {
		t.Errorf("IBAN import = %+v, want account acct1 matched", rep)
	}
}

// Re-importing the same document returns the original report with
// Duplicate set — a re-uploaded confirmation never double-counts.
func TestImportMT900910Duplicate(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "dup-e1", "bank", "acct1", "mt103-DUP1", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	doc := mt900910Doc(t, "900", "CONF-DUP", "DUP1", "acct1",
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 10000)

	first, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
	second, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("second import: duplicate = false, want true")
	}
	if second.DocumentSHA256 != first.DocumentSHA256 || second.Match.Status != first.Match.Status {
		t.Errorf("duplicate report differs from the original: %+v vs %+v", second, first)
	}
}

// :52A: and :72: ride along in the report verbatim.
func TestImportMT900910BICAndInfo(t *testing.T) {
	l := New()
	mt900910Seed(t, l, "bi-e1", "bank", "acct1", "mt103-BI1", 10000,
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), false)
	doc, err := ExportMT900910(MT900910{
		MessageType: "900", SenderLT: "BANKUS33", ReceiverLT: "BANKDEFF",
		TransactionReference: "CONF-BI", RelatedReference: "BI1", Account: "acct1",
		ValueDate: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		Currency: "USD", AmountCents: 10000,
		OrderingInstitutionBIC: "DEUTDEFF",
		SenderToReceiverInfo:   []string{"INVOICE 42 PAID"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := l.ImportMT900910("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT900910: %v", err)
	}
	if rep.OrderingInstitutionBIC != "DEUTDEFF" {
		t.Errorf("ordering BIC = %q, want DEUTDEFF", rep.OrderingInstitutionBIC)
	}
	if len(rep.SenderToReceiverInfo) != 1 || rep.SenderToReceiverInfo[0] != "INVOICE 42 PAID" {
		t.Errorf("sender-to-receiver info = %q, want [INVOICE 42 PAID]", rep.SenderToReceiverInfo)
	}
	if rep.Match.Status != CAMT053Matched {
		t.Errorf("match = %+v, want matched", rep.Match)
	}
}
