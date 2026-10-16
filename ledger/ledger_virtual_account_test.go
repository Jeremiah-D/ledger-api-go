package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustRegisterVAN(t *testing.T, l *Ledger, van, parent string) {
	t.Helper()
	if err := l.RegisterVirtualAccount(AccountID(van), AccountID(parent)); err != nil {
		t.Fatalf("RegisterVirtualAccount(%s -> %s): %v", van, parent, err)
	}
}

// Registration validation: the minimal safe set.
func TestVANRegistrationValidation(t *testing.T) {
	l := New()
	if err := l.RegisterVirtualAccount("", "p"); !errors.Is(err, ErrEmptyAccountID) {
		t.Errorf("empty van: got %v, want ErrEmptyAccountID", err)
	}
	if err := l.RegisterVirtualAccount("v", ""); !errors.Is(err, ErrVANEmptyParent) {
		t.Errorf("empty parent: got %v, want ErrVANEmptyParent", err)
	}
	if err := l.RegisterVirtualAccount("v", "v"); !errors.Is(err, ErrVANSelfParent) {
		t.Errorf("self parent: got %v, want ErrVANSelfParent", err)
	}

	mustRegisterVAN(t, l, "v1", "p1")
	// The parent must not itself be a VAN: no chains.
	if err := l.RegisterVirtualAccount("v2", "v1"); !errors.Is(err, ErrVANParentIsVAN) {
		t.Errorf("parent-is-VAN: got %v, want ErrVANParentIsVAN", err)
	}
	// An account that already parents a VAN cannot itself become one.
	if err := l.RegisterVirtualAccount("p1", "p2"); !errors.Is(err, ErrVANHasAttributedChildren) {
		t.Errorf("van-already-parents: got %v, want ErrVANHasAttributedChildren", err)
	}
	// Failed registrations change nothing.
	if got := l.VirtualAccountVersion(); got != 1 {
		t.Errorf("version = %d, want 1 after failed registrations", got)
	}
	if _, ok := l.VirtualAccountParent("v2"); ok {
		t.Error("v2 should not be registered after rejected parent-is-VAN")
	}
}

// The mapping version bumps on every change, never on idempotent
// no-ops.
func TestVANVersionBumps(t *testing.T) {
	l := New()
	if got := l.VirtualAccountVersion(); got != 0 {
		t.Fatalf("initial version = %d, want 0", got)
	}
	mustRegisterVAN(t, l, "v1", "p1")
	mustRegisterVAN(t, l, "v1", "p1") // idempotent: no bump
	if got := l.VirtualAccountVersion(); got != 1 {
		t.Fatalf("version = %d, want 1 after idempotent re-register", got)
	}
	mustRegisterVAN(t, l, "v1", "p2") // changed parent: bump
	if got := l.VirtualAccountVersion(); got != 2 {
		t.Fatalf("version = %d, want 2 after re-parent", got)
	}
	if p, _ := l.VirtualAccountParent("v1"); p != "p2" {
		t.Fatalf("parent = %q, want p2", p)
	}
	if err := l.UnregisterVirtualAccount("v1"); err != nil {
		t.Fatalf("UnregisterVirtualAccount: %v", err)
	}
	if got := l.VirtualAccountVersion(); got != 3 {
		t.Fatalf("version = %d, want 3 after unregister", got)
	}
	if err := l.UnregisterVirtualAccount("v1"); err != nil { // no-op
		t.Fatalf("UnregisterVirtualAccount (unknown): %v", err)
	}
	if got := l.VirtualAccountVersion(); got != 3 {
		t.Fatalf("version = %d, want 3 after no-op unregister", got)
	}
	if _, ok := l.VirtualAccountParent("v1"); ok {
		t.Error("v1 should be gone after unregister")
	}
	if err := l.UnregisterVirtualAccount(""); !errors.Is(err, ErrEmptyAccountID) {
		t.Errorf("empty van unregister: got %v, want ErrEmptyAccountID", err)
	}
}

// ListVirtualAccounts returns mappings sorted by VAN.
func TestVANListSorted(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "v3", "p3")
	mustRegisterVAN(t, l, "v1", "p1")
	mustRegisterVAN(t, l, "v2", "p2")
	got := l.ListVirtualAccounts()
	if len(got) != 3 || got[0].VAN != "v1" || got[1].VAN != "v2" || got[2].VAN != "v3" {
		t.Fatalf("list not sorted by VAN: %+v", got)
	}
}

// Post to a VAN: the parent is booked, the VAN stays at zero, the
// journal row carries the attribution marker.
func TestVANPostAttribution(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustRegisterVAN(t, l, "van-1", "parent-1")

	e, dup, err := l.Post(JournalEntry{
		ID: "vp1", DebitAccount: "van-1", CreditAccount: "payer",
		AmountCents: 5000, Currency: "USD", Memo: "order-42",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if dup {
		t.Fatal("unexpected duplicate")
	}
	if e.DebitAccount != "parent-1" {
		t.Errorf("booked debit leg = %q, want parent-1", e.DebitAccount)
	}
	if e.Memo != "order-42;van-attribution:van-1" {
		t.Errorf("memo = %q, want caller memo + attribution marker", e.Memo)
	}
	if got := l.BalanceIn("parent-1", "USD"); got != 5000 {
		t.Errorf("parent balance = %d, want 5000", got)
	}
	if got := l.BalanceIn("van-1", "USD"); got != 0 {
		t.Errorf("VAN balance = %d, want 0 (zero-balance pass-through)", got)
	}
	// The journaled row is the attributed one.
	stored := l.entries["vp1"]
	if stored.DebitAccount != "parent-1" || !strings.Contains(stored.Memo, "van-attribution:van-1") {
		t.Errorf("journaled row not attributed: %+v", stored)
	}

	// Audit: one post event plus one virtual_account_attribution event
	// with the van/parent/amount/currency details.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var attr *AuditEvent
	for i, ev := range events {
		if ev.Op == "virtual_account_attribution" {
			attr = &events[i]
		}
	}
	if attr == nil {
		t.Fatalf("no virtual_account_attribution event; ops: %v", opNames(events))
	}
	if attr.Actor != "Post" || attr.TraceID != "vp1" {
		t.Errorf("actor/trace = %q/%q, want Post/vp1", attr.Actor, attr.TraceID)
	}
	if len(attr.Accounts) != 2 || attr.Accounts[0] != "van-1" || attr.Accounts[1] != "parent-1" {
		t.Errorf("accounts = %v, want [van-1 parent-1]", attr.Accounts)
	}
	d := attr.Details
	if d["van"] != "van-1" || d["parent"] != "parent-1" || d["amount_cents"] != float64(5000) || d["currency"] != "USD" {
		t.Errorf("details = %v, want van/parent/amount_cents/currency", d)
	}
	if attr.VersionBefore != attr.VersionAfter {
		t.Errorf("attribution moved version %d -> %d", attr.VersionBefore, attr.VersionAfter)
	}
	if len(attr.EntryIDs) != 1 || attr.EntryIDs[0] != "vp1" {
		t.Errorf("entry_ids = %v, want [vp1]", attr.EntryIDs)
	}
}

// A VAN on the paying leg is rejected: it holds no balance.
func TestVANNoDebit(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "van-1", "parent-1")

	if _, _, err := l.Post(JournalEntry{ID: "vd1", DebitAccount: "payer", CreditAccount: "van-1", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrVANNoDebit) {
		t.Errorf("Post paying from VAN: got %v, want ErrVANNoDebit", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "vd2", From: "van-1", To: "payer", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrVANNoDebit) {
		t.Errorf("PostTransfer from VAN: got %v, want ErrVANNoDebit", err)
	}
	// Rejected postings record nothing.
	if l.Version() != 0 {
		t.Errorf("version = %d, want 0 after rejected VAN debits", l.Version())
	}
}

// PostTransfer to a VAN: the principal books to the parent.
func TestVANTransferAttribution(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "van-1", "parent-1")
	rcpt, err := l.PostTransfer(Transfer{ID: "vt1", From: "payer", To: "van-1", AmountCents: 2500, Currency: "USD", Memo: "invoice-7"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(rcpt.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(rcpt.Entries))
	}
	principal := rcpt.Entries[0]
	if principal.DebitAccount != "parent-1" {
		t.Errorf("principal debit leg = %q, want parent-1", principal.DebitAccount)
	}
	if principal.Memo != "invoice-7;van-attribution:van-1" {
		t.Errorf("principal memo = %q", principal.Memo)
	}
	if got := l.BalanceIn("parent-1", "USD"); got != 2500 {
		t.Errorf("parent balance = %d, want 2500", got)
	}
	if got := l.BalanceIn("van-1", "USD"); got != 0 {
		t.Errorf("VAN balance = %d, want 0", got)
	}
	if n := l.VANAttributionCount(); n != 1 {
		t.Errorf("attribution count = %d, want 1", n)
	}
}

// The idempotency replay check runs before VAN validation: replaying a
// key posted before the VAN was registered returns the original entry,
// not an attribution or a rejection.
func TestVANReplayBeforeValidation(t *testing.T) {
	l := New()
	orig, _, err := l.Post(JournalEntry{ID: "vr1", DebitAccount: "acct", CreditAccount: "payer", AmountCents: 100, Currency: "USD", IdempotencyKey: "k-vr"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	// Register the VANs after the original posting.
	mustRegisterVAN(t, l, "acct", "parent-1")  // receiving leg now a VAN
	mustRegisterVAN(t, l, "payer", "parent-2") // paying leg now a VAN

	// Replay of the receiving leg returns the original untouched.
	rep, dup, err := l.Post(JournalEntry{ID: "vr2", DebitAccount: "acct", CreditAccount: "other", AmountCents: 100, Currency: "USD", IdempotencyKey: "k-vr"})
	if err != nil {
		t.Fatalf("replay Post: %v", err)
	}
	if !dup || rep.ID != orig.ID || rep.DebitAccount != "acct" {
		t.Errorf("replay should return the original entry unchanged: %+v", rep)
	}
	// Replay of the paying leg also returns the original, not a
	// rejection — the replay books nothing new.
	rep, dup, err = l.Post(JournalEntry{ID: "vr3", DebitAccount: "other", CreditAccount: "payer", AmountCents: 100, Currency: "USD", IdempotencyKey: "k-vr"})
	if err != nil {
		t.Fatalf("replay Post (payer VAN): %v", err)
	}
	if !dup || rep.ID != orig.ID {
		t.Errorf("replay should return the original entry: %+v", rep)
	}
	// But a FRESH key through the now-registered VAN attributes.
	if _, _, err := l.Post(JournalEntry{ID: "vr4", DebitAccount: "acct", CreditAccount: "other", AmountCents: 100, Currency: "USD", IdempotencyKey: "k-vr4"}); err != nil {
		t.Fatalf("fresh Post after VAN registration: %v", err)
	}
	if got := l.BalanceIn("parent-1", "USD"); got != 100 {
		t.Errorf("parent-1 balance = %d, want 100", got)
	}
}

// Frozen/overdraft gates see the parent, not the VAN: freezing the
// parent rejects VAN receipts.
func TestVANRiskGatesSeeParent(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "van-1", "parent-1")
	l.Freeze("parent-1")
	if _, _, err := l.Post(JournalEntry{ID: "vg1", DebitAccount: "van-1", CreditAccount: "payer", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("VAN receipt to frozen parent: got %v, want ErrAccountFrozen", err)
	}
	l.Unfreeze("parent-1")
	if _, _, err := l.Post(JournalEntry{ID: "vg2", DebitAccount: "van-1", CreditAccount: "payer", AmountCents: 100, Currency: "USD"}); err != nil {
		t.Errorf("VAN receipt after unfreeze: %v", err)
	}
}

// Batch entries attribute per entry; a VAN debit anywhere rejects the
// whole batch.
func TestVANBatchAttribution(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "van-1", "parent-1")
	rcpt, err := l.PostBatch(Batch{ID: "vb1", Entries: []JournalEntry{
		{ID: "vb1e1", DebitAccount: "van-1", CreditAccount: "payer", AmountCents: 300, Currency: "USD"},
		{ID: "vb1e2", DebitAccount: "other", CreditAccount: "payer", AmountCents: 200, Currency: "USD"},
	}})
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if rcpt.Entries[0].DebitAccount != "parent-1" {
		t.Errorf("batch entry debit leg = %q, want parent-1", rcpt.Entries[0].DebitAccount)
	}
	if got := l.BalanceIn("parent-1", "USD"); got != 300 {
		t.Errorf("parent balance = %d, want 300", got)
	}
	if got := l.BalanceIn("van-1", "USD"); got != 0 {
		t.Errorf("VAN balance = %d, want 0", got)
	}

	_, err = l.PostBatch(Batch{ID: "vb2", Entries: []JournalEntry{
		{ID: "vb2e1", DebitAccount: "other", CreditAccount: "payer", AmountCents: 100, Currency: "USD"},
		{ID: "vb2e2", DebitAccount: "other", CreditAccount: "van-1", AmountCents: 100, Currency: "USD"},
	}})
	if !errors.Is(err, ErrVANNoDebit) {
		t.Errorf("batch with VAN debit: got %v, want ErrVANNoDebit", err)
	}
}

// Split chunks attribute per chunk.
func TestVANSplitAttribution(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	mustRegisterVAN(t, l, "van-1", "parent-1")
	rcpt, err := l.PostTransferSplit(Transfer{ID: "vs1", From: "payer", To: "van-1", AmountCents: 25000, Currency: "USD"})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	for _, e := range rcpt.Entries {
		if strings.HasSuffix(e.ID, "/fee") {
			continue
		}
		if e.DebitAccount != "parent-1" {
			t.Errorf("chunk %s debit leg = %q, want parent-1", e.ID, e.DebitAccount)
		}
		if !strings.Contains(e.Memo, "van-attribution:van-1") {
			t.Errorf("chunk %s missing attribution memo: %q", e.ID, e.Memo)
		}
	}
	if got := l.BalanceIn("parent-1", "USD"); got != 25000 {
		t.Errorf("parent balance = %d, want 25000", got)
	}
	if got := l.BalanceIn("van-1", "USD"); got != 0 {
		t.Errorf("VAN balance = %d, want 0", got)
	}
}

// The fee leg attributes when the fee account is a VAN.
func TestVANFeeLegAttribution(t *testing.T) {
	l := New(WithTransferFeePolicy(1000, "fee-van")) // 10% to fee-van
	mustRegisterVAN(t, l, "fee-van", "fee-parent")
	mustRegisterVAN(t, l, "van-1", "parent-1")
	rcpt, err := l.PostTransfer(Transfer{ID: "vf1", From: "payer", To: "van-1", AmountCents: 10000, Currency: "USD"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.FeeCents != 1000 {
		t.Fatalf("fee = %d, want 1000", rcpt.FeeCents)
	}
	var feeLeg *JournalEntry
	for i, e := range rcpt.Entries {
		if e.ID == "vf1/fee" {
			feeLeg = &rcpt.Entries[i]
		}
	}
	if feeLeg == nil {
		t.Fatal("fee leg missing")
	}
	if feeLeg.DebitAccount != "fee-parent" {
		t.Errorf("fee leg debit = %q, want fee-parent", feeLeg.DebitAccount)
	}
	if !strings.Contains(feeLeg.Memo, "van-attribution:fee-van") {
		t.Errorf("fee leg missing attribution memo: %q", feeLeg.Memo)
	}
	if got := l.BalanceIn("fee-parent", "USD"); got != 1000 {
		t.Errorf("fee-parent balance = %d, want 1000", got)
	}
}

// Snapshot round-trip: the mappings and the version survive, and
// attribution still works after a restore.
func TestVANSnapshotRoundTrip(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "van-1", "parent-1")
	mustRegisterVAN(t, l, "van-2", "parent-2")
	if err := l.UnregisterVirtualAccount("van-2"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	wantVersion := l.VirtualAccountVersion()

	var sb strings.Builder
	if err := l.ExportSnapshot(&sb); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.VirtualAccountVersion(); got != wantVersion {
		t.Errorf("restored version = %d, want %d", got, wantVersion)
	}
	if p, ok := l2.VirtualAccountParent("van-1"); !ok || p != "parent-1" {
		t.Errorf("restored mapping van-1 -> %q, %v", p, ok)
	}
	if _, ok := l2.VirtualAccountParent("van-2"); ok {
		t.Error("van-2 should stay unregistered after round-trip")
	}
	// Attribution still works on the restored ledger.
	if _, _, err := l2.Post(JournalEntry{ID: "vs1", DebitAccount: "van-1", CreditAccount: "payer", AmountCents: 50, Currency: "USD"}); err != nil {
		t.Fatalf("Post after restore: %v", err)
	}
	if got := l2.BalanceIn("parent-1", "USD"); got != 50 {
		t.Errorf("restored parent balance = %d, want 50", got)
	}
	// A corrupt mapping fails the import instead of installing it.
	bad := strings.Replace(sb.String(), `"van":"van-1"`, `"van":""`, 1)
	if _, err := ImportSnapshot(strings.NewReader(bad)); err == nil {
		t.Error("import of snapshot with empty VAN should fail")
	}
}

// Reconcile discloses the mappings and the version.
func TestVANReconcileDisclosure(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "van-b", "parent-b")
	mustRegisterVAN(t, l, "van-a", "parent-a")
	report := l.Reconcile(time.Now())
	if report.VirtualAccountVersion != 2 {
		t.Errorf("report version = %d, want 2", report.VirtualAccountVersion)
	}
	if len(report.VirtualAccounts) != 2 || report.VirtualAccounts[0].VAN != "van-a" || report.VirtualAccounts[1].VAN != "van-b" {
		t.Errorf("report mappings = %+v, want sorted [van-a van-b]", report.VirtualAccounts)
	}
}

// Dry-run through a VAN: the simulation attributes without touching
// the live ledger.
func TestVANDryRun(t *testing.T) {
	l := New()
	mustRegisterVAN(t, l, "van-1", "parent-1")
	preview, err := l.DryRunTransfer(Transfer{ID: "vdry", From: "payer", To: "van-1", AmountCents: 100, Currency: "USD"})
	if err != nil {
		t.Fatalf("DryRunTransfer: %v", err)
	}
	_ = preview
	if got := l.BalanceIn("parent-1", "USD"); got != 0 {
		t.Errorf("live parent balance moved on dry run: %d", got)
	}
	if l.Version() != 0 {
		t.Errorf("live version moved on dry run: %d", l.Version())
	}
}

// Cross-currency transfer to a VAN: the payee leg attributes the
// converted amount in the settlement currency.
func TestVANFXTransferAttribution(t *testing.T) {
	l := New(WithFXAccount("fx-clear"))
	mustSetRate(t, l, "USD", "EUR", 9, 10) // 1 USD = 0.9 EUR
	mustRegisterVAN(t, l, "van-1", "parent-1")
	rcpt, err := l.PostTransfer(Transfer{ID: "vfx1", From: "payer", To: "van-1", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"})
	if err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	if rcpt.FX == nil || rcpt.FX.ConvertedCents != 9000 {
		t.Fatalf("converted = %+v, want 9000", rcpt.FX)
	}
	// The payee leg books the converted amount to the parent.
	var payeeLeg *JournalEntry
	for i, e := range rcpt.Entries {
		if e.ID == "vfx1" {
			payeeLeg = &rcpt.Entries[i]
		}
	}
	if payeeLeg == nil {
		t.Fatal("payee leg missing")
	}
	if payeeLeg.DebitAccount != "parent-1" || payeeLeg.AmountCents != 9000 || payeeLeg.Currency != "EUR" {
		t.Errorf("payee leg = %+v, want debit parent-1 9000 EUR", payeeLeg)
	}
	if !strings.Contains(payeeLeg.Memo, "van-attribution:van-1") {
		t.Errorf("payee leg missing attribution memo: %q", payeeLeg.Memo)
	}
	if got := l.BalanceIn("parent-1", "EUR"); got != 9000 {
		t.Errorf("parent EUR balance = %d, want 9000", got)
	}
	if got := l.BalanceIn("van-1", "EUR"); got != 0 {
		t.Errorf("VAN EUR balance = %d, want 0", got)
	}
	// The FX clearing account can never be a VAN: it counters both
	// legs, and a zero-balance account cannot clear.
	mustRegisterVAN(t, l, "van-clear", "parent-clear")
	if _, err := l.PostTransfer(Transfer{ID: "vfx2", From: "payer", To: "other", AmountCents: 100, Currency: "USD", ToCurrency: "EUR", FXAccount: "van-clear"}); !errors.Is(err, ErrVANNoDebit) {
		t.Errorf("VAN clearing account: got %v, want ErrVANNoDebit", err)
	}
	// A VAN payer is rejected on the FX path too.
	if _, err := l.PostTransfer(Transfer{ID: "vfx3", From: "van-1", To: "other", AmountCents: 100, Currency: "USD", ToCurrency: "EUR"}); !errors.Is(err, ErrVANNoDebit) {
		t.Errorf("VAN payer on FX path: got %v, want ErrVANNoDebit", err)
	}
}
