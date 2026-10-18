package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// postLinkFixture posts two plain entries and returns the ledger.
func postLinkFixture(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	for _, id := range []string{"pay-1", "pay-2"} {
		if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: "alice", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"}); err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	return l
}

func TestDeclareEntryLink(t *testing.T) {
	l := postLinkFixture(t)
	for _, linkType := range []string{LinkTypeReversalOf, LinkTypeAdjustmentOf, LinkTypeFeeOf, LinkTypeRefundOf} {
		from := "pay-1/" + linkType
		if _, _, err := l.Post(JournalEntry{ID: from, DebitAccount: "bank", CreditAccount: "alice", AmountCents: 100, Currency: "USD"}); err != nil {
			t.Fatalf("Post %s: %v", from, err)
		}
		link, declared, err := l.DeclareEntryLink(from, "pay-1", linkType)
		if err != nil {
			t.Fatalf("DeclareEntryLink(%s): %v", linkType, err)
		}
		if !declared {
			t.Errorf("DeclareEntryLink(%s): declared = false, want true", linkType)
		}
		if link.FromEntryID != from || link.ToEntryID != "pay-1" || link.LinkType != linkType {
			t.Errorf("link = %+v, want from=%s to=pay-1 type=%s", link, from, linkType)
		}
		if link.DeclaredAt.IsZero() {
			t.Errorf("link %s: DeclaredAt is zero", linkType)
		}
		if link.DeclaredBy != "" {
			t.Errorf("link %s: DeclaredBy = %q, want empty (no actor context)", linkType, link.DeclaredBy)
		}
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after declarations: %v", err)
	}
}

func TestDeclareEntryLinkValidation(t *testing.T) {
	l := postLinkFixture(t)
	cases := []struct {
		name     string
		from     string
		to       string
		linkType string
		want     error
	}{
		{"bad type", "pay-1", "pay-2", "corrects", ErrInvalidLinkType},
		{"empty type", "pay-1", "pay-2", "", ErrInvalidLinkType},
		{"case sensitive", "pay-1", "pay-2", "REVERSAL_OF", ErrInvalidLinkType},
		{"empty from", "", "pay-2", LinkTypeReversalOf, ErrEmptyLinkEntryID},
		{"empty to", "pay-1", "", LinkTypeReversalOf, ErrEmptyLinkEntryID},
		{"unknown from", "nope", "pay-2", LinkTypeReversalOf, ErrEntryNotFound},
		{"unknown to", "pay-1", "nope", LinkTypeReversalOf, ErrEntryNotFound},
		{"self link", "pay-1", "pay-1", LinkTypeReversalOf, ErrLinkSelfReference},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := l.DeclareEntryLink(tc.from, tc.to, tc.linkType); !errors.Is(err, tc.want) {
				t.Errorf("DeclareEntryLink(%q,%q,%q) = %v, want %v", tc.from, tc.to, tc.linkType, err, tc.want)
			}
		})
	}
	if n := len(l.entryLinks); n != 0 {
		t.Errorf("entryLinks = %d after all rejections, want 0", n)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after rejections: %v", err)
	}
}

func TestDeclareEntryLinkIdempotent(t *testing.T) {
	l := postLinkFixture(t)
	first, declared, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeAdjustmentOf)
	if err != nil {
		t.Fatalf("DeclareEntryLink: %v", err)
	}
	if !declared {
		t.Fatal("declared = false on first declaration")
	}
	// The identical triple replays silently: same link, declared=false,
	// no new chain link, no counter bump.
	second, declared, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeAdjustmentOf)
	if err != nil {
		t.Fatalf("redeclare: %v", err)
	}
	if declared {
		t.Error("redeclare: declared = true, want false")
	}
	if second.FromEntryID != first.FromEntryID || second.DeclaredAt != first.DeclaredAt {
		t.Errorf("redeclare returned %+v, want the original %+v", second, first)
	}
	if n := len(l.entryLinks); n != 1 {
		t.Errorf("entryLinks = %d, want 1", n)
	}
	if n := len(l.linkChain); n != 1 {
		t.Errorf("linkChain = %d, want 1", n)
	}
	if c := l.EntryLinkChangeCount(); c != 1 {
		t.Errorf("EntryLinkChangeCount = %d, want 1", c)
	}
	// A different type on the same pair is a distinct declaration.
	if _, declared, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeRefundOf); err != nil || !declared {
		t.Errorf("distinct type declare = (%v, %v), want (true, nil)", declared, err)
	}
	if c := l.EntryLinkChangeCount(); c != 2 {
		t.Errorf("EntryLinkChangeCount = %d, want 2", c)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestDeclareEntryLinkCtxActor(t *testing.T) {
	l := postLinkFixture(t)
	ctx := ContextWithActor(context.Background(), "ops-li")
	link, _, err := l.DeclareEntryLinkCtx(ctx, "pay-1", "pay-2", LinkTypeAdjustmentOf)
	if err != nil {
		t.Fatalf("DeclareEntryLinkCtx: %v", err)
	}
	if link.DeclaredBy != "ops-li" {
		t.Errorf("DeclaredBy = %q, want ops-li", link.DeclaredBy)
	}
}

// TestEntryLinkChainTamper rewrites a declared link behind the chain's
// back: VerifyChain must fail, exactly like a rewritten journal entry.
func TestEntryLinkChainTamper(t *testing.T) {
	l := postLinkFixture(t)
	if _, _, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeAdjustmentOf); err != nil {
		t.Fatalf("DeclareEntryLink: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain before tamper: %v", err)
	}
	// Rewrite the link's endpoint: the recomputed hash no longer matches
	// the recorded chain link.
	l.entryLinks[0].ToEntryID = "pay-1"
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed after link tamper, want failure")
	} else {
		t.Logf("tampered endpoint: %v", err)
	}
	// Restore and tamper the type instead.
	l.entryLinks[0].ToEntryID = "pay-2"
	l.entryLinks[0].LinkType = LinkTypeRefundOf
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed after type tamper, want failure")
	}
	// Restore and tamper the declaration metadata.
	l.entryLinks[0].LinkType = LinkTypeAdjustmentOf
	l.entryLinks[0].DeclaredBy = "mallory"
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed after declarer tamper, want failure")
	}
	l.entryLinks[0].DeclaredBy = ""
	// Tamper the recorded chain hash itself: a splice is detected too.
	l.linkChain[0].hash[0] ^= 0xff
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed after chain-hash tamper, want failure")
	}
}

// TestEntryLinkChainGenesis: a ledger with no links verifies cleanly,
// and the first declaration chains from the zero genesis.
func TestEntryLinkChainGenesis(t *testing.T) {
	l := postLinkFixture(t)
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain with no links: %v", err)
	}
	if _, _, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeFeeOf); err != nil {
		t.Fatalf("DeclareEntryLink: %v", err)
	}
	var genesis [32]byte
	if l.linkChain[0].prevHash != genesis {
		t.Error("first link-chain link does not extend the zero genesis")
	}
	if l.linkChain[0].seq != 1 {
		t.Errorf("first link-chain seq = %d, want 1", l.linkChain[0].seq)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after first declaration: %v", err)
	}
}

func TestReverseEntryDeclaresLink(t *testing.T) {
	l := postLinkFixture(t)
	if _, _, err := l.ReverseEntry("pay-1", ""); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	// The reversal entry carries an outgoing reversal_of link to the original.
	out := l.EntryLinks("pay-1/reversal")
	if len(out) != 1 {
		t.Fatalf("EntryLinks(pay-1/reversal) = %d links, want 1", len(out))
	}
	if out[0].LinkType != LinkTypeReversalOf || out[0].Direction != LinkDirectionOutgoing {
		t.Errorf("link = %+v, want reversal_of/outgoing", out[0])
	}
	if out[0].ToEntryID != "pay-1" {
		t.Errorf("link ToEntryID = %q, want pay-1", out[0].ToEntryID)
	}
	// The original sees the same link as incoming.
	in := l.EntryLinks("pay-1")
	if len(in) != 1 || in[0].Direction != LinkDirectionIncoming {
		t.Fatalf("EntryLinks(pay-1) = %+v, want 1 incoming link", in)
	}
	if c := l.EntryLinkChangeCount(); c != 1 {
		t.Errorf("EntryLinkChangeCount = %d, want 1", c)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestReverseEntryPartialDeclaresLink(t *testing.T) {
	l := postLinkFixture(t)
	if _, _, err := l.ReverseEntryPartial("pay-1", 400, ""); err != nil {
		t.Fatalf("ReverseEntryPartial: %v", err)
	}
	out := l.EntryLinks("pay-1/reversal/1")
	if len(out) != 1 || out[0].LinkType != LinkTypeReversalOf || out[0].ToEntryID != "pay-1" {
		t.Fatalf("EntryLinks(pay-1/reversal/1) = %+v, want 1 reversal_of link to pay-1", out)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

// TestReverseEntryDuplicateNoDoubleDeclare: an idempotent reversal replay
// books nothing new — and declares no second link.
func TestReverseEntryDuplicateNoDoubleDeclare(t *testing.T) {
	l := postLinkFixture(t)
	if _, _, err := l.ReverseEntry("pay-1", ""); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if _, dup, err := l.ReverseEntry("pay-1", ""); err != nil || !dup {
		t.Fatalf("replay ReverseEntry = (dup=%v, err=%v), want (true, nil)", dup, err)
	}
	if n := len(l.entryLinks); n != 1 {
		t.Errorf("entryLinks = %d after replay, want 1", n)
	}
	if c := l.EntryLinkChangeCount(); c != 1 {
		t.Errorf("EntryLinkChangeCount = %d after replay, want 1", c)
	}
}

// TestEntryLinksBidirectional: one entry can be both a From and a To,
// each annotated with its direction, in declaration order.
func TestEntryLinksBidirectional(t *testing.T) {
	l := postLinkFixture(t)
	if _, _, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeAdjustmentOf); err != nil {
		t.Fatalf("declare 1: %v", err)
	}
	if _, _, err := l.DeclareEntryLink("pay-2", "pay-1", LinkTypeRefundOf); err != nil {
		t.Fatalf("declare 2: %v", err)
	}
	got := l.EntryLinks("pay-1")
	if len(got) != 2 {
		t.Fatalf("EntryLinks(pay-1) = %d, want 2", len(got))
	}
	if got[0].Direction != LinkDirectionOutgoing || got[0].LinkType != LinkTypeAdjustmentOf {
		t.Errorf("link 0 = %+v, want outgoing adjustment_of", got[0])
	}
	if got[1].Direction != LinkDirectionIncoming || got[1].LinkType != LinkTypeRefundOf {
		t.Errorf("link 1 = %+v, want incoming refund_of", got[1])
	}
	// An entry with no links returns an empty (non-nil) list.
	if links := l.EntryLinks("pay-2"); len(links) != 2 {
		t.Errorf("EntryLinks(pay-2) = %d, want 2", len(links))
	}
	if _, _, err := l.Post(JournalEntry{ID: "lonely", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 1, Currency: "USD"}); err != nil {
		t.Fatalf("Post lonely: %v", err)
	}
	if links := l.EntryLinks("lonely"); links == nil || len(links) != 0 {
		t.Errorf("EntryLinks(lonely) = %v, want empty non-nil list", links)
	}
}

func TestEntryExists(t *testing.T) {
	l := postLinkFixture(t)
	if !l.EntryExists("pay-1") {
		t.Error("EntryExists(pay-1) = false, want true")
	}
	if l.EntryExists("nope") {
		t.Error("EntryExists(nope) = true, want false")
	}
}

// TestReconcileLinkChains: reversal_of/adjustment_of links expand into
// per-root adjustment chains on the Reconcile report.
func TestReconcileLinkChains(t *testing.T) {
	l := postLinkFixture(t)
	// b adjusts a; then a is reversed (auto-declares reversal_of); then
	// the reversal itself is reversed (reversal of the reversal).
	if _, _, err := l.DeclareEntryLink("pay-2", "pay-1", LinkTypeAdjustmentOf); err != nil {
		t.Fatalf("declare adjustment: %v", err)
	}
	if _, _, err := l.ReverseEntry("pay-1", ""); err != nil {
		t.Fatalf("ReverseEntry(pay-1): %v", err)
	}
	if _, _, err := l.ReverseEntry("pay-1/reversal", ""); err != nil {
		t.Fatalf("ReverseEntry(pay-1/reversal): %v", err)
	}
	// A fee_of link is disclosed flat but stays out of the chains.
	if _, _, err := l.DeclareEntryLink("pay-2", "pay-1", LinkTypeFeeOf); err != nil {
		t.Fatalf("declare fee: %v", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.EntryLinks) != 4 {
		t.Fatalf("report.EntryLinks = %d, want 4", len(report.EntryLinks))
	}
	if len(report.LinkChains) != 1 {
		t.Fatalf("report.LinkChains = %d, want 1", len(report.LinkChains))
	}
	chain := report.LinkChains[0]
	if chain.RootEntryID != "pay-1" {
		t.Errorf("chain root = %q, want pay-1", chain.RootEntryID)
	}
	if len(chain.Links) != 3 {
		t.Fatalf("chain links = %d, want 3 (adjustment + 2 reversals)", len(chain.Links))
	}
	wantTypes := []string{LinkTypeAdjustmentOf, LinkTypeReversalOf, LinkTypeReversalOf}
	for i, wt := range wantTypes {
		if chain.Links[i].LinkType != wt {
			t.Errorf("chain link %d type = %q, want %q", i, chain.Links[i].LinkType, wt)
		}
	}
	// The fee_of link is not in any chain.
	for _, c := range report.LinkChains {
		for _, link := range c.Links {
			if link.LinkType == LinkTypeFeeOf {
				t.Errorf("fee_of link leaked into chain %q", c.RootEntryID)
			}
		}
	}
}

// TestEntryLinkSnapshotRoundTrip: links and their chain survive an
// export/import cycle, and the restored ledger verifies.
func TestEntryLinkSnapshotRoundTrip(t *testing.T) {
	l := postLinkFixture(t)
	if _, _, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeAdjustmentOf); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, _, err := l.ReverseEntry("pay-2", ""); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("snapshot round-trip diverged on entry links")
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on restored ledger: %v", err)
	}
	if got := restored.EntryLinks("pay-1"); len(got) != 1 || got[0].Direction != LinkDirectionOutgoing {
		t.Errorf("restored EntryLinks(pay-1) = %+v, want 1 outgoing", got)
	}
	// A snapshot whose link names a missing entry is rejected, not
	// half-installed.
	bad := postLinkFixture(t)
	bad.entryLinks = []EntryLink{{FromEntryID: "pay-1", ToEntryID: "ghost", LinkType: LinkTypeFeeOf, DeclaredAt: time.Now().UTC()}}
	var badBuf bytes.Buffer
	if err := bad.ExportSnapshot(&badBuf); err != nil {
		t.Fatalf("ExportSnapshot(bad): %v", err)
	}
	if _, err := ImportSnapshot(&badBuf); err == nil {
		t.Error("ImportSnapshot accepted a link to a missing entry, want rejection")
	}
}

// TestEntryLinkAuditEvent: declarations are recorded as
// entry_link_declared audit events — both explicit declarations and the
// reversal paths' automatic links.
func TestEntryLinkAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	for _, id := range []string{"pay-1", "pay-2"} {
		if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: "alice", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"}); err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	if _, _, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeRefundOf); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, _, err := l.ReverseEntry("pay-2", ""); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var found int
	for _, ev := range events {
		if ev.Op != "entry_link_declared" {
			continue
		}
		found++
		if len(ev.EntryIDs) != 2 {
			t.Errorf("entry_link_declared event entry_ids = %v, want 2", ev.EntryIDs)
		}
		if ev.VersionBefore != ev.VersionAfter {
			t.Errorf("entry_link_declared event bumped version %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
		}
	}
	// One explicit declaration plus the reversal's automatic link.
	if found != 2 {
		t.Errorf("entry_link_declared events = %d, want 2", found)
	}
}

// TestEntryLinkConcurrentDeclare hammers DeclareEntryLink from goroutines:
// the registry and the chain stay consistent under -race.
func TestEntryLinkConcurrentDeclare(t *testing.T) {
	l := New()
	for i := 0; i < 8; i++ {
		id := string(rune('a'+i)) + "-entry"
		if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: "alice", CreditAccount: "bank", AmountCents: 10, Currency: "USD"}); err != nil {
			t.Fatalf("Post: %v", err)
		}
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			from := string(rune('a'+i)) + "-entry"
			to := string(rune('a'+(i+1)%8)) + "-entry"
			if _, _, err := l.DeclareEntryLink(from, to, LinkTypeAdjustmentOf); err != nil {
				t.Errorf("DeclareEntryLink: %v", err)
			}
			_ = l.EntryLinks(from)
			_ = l.Reconcile(time.Now())
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after concurrent declares: %v", err)
	}
	if c := l.EntryLinkChangeCount(); c != 8 {
		t.Errorf("EntryLinkChangeCount = %d, want 8", c)
	}
}

// TestEntryLinkDryRun: the dry-run twin clones the link registry and
// chain — a what-if post on a ledger with declared links simulates
// against the same association record, and the live ledger is untouched.
func TestEntryLinkDryRun(t *testing.T) {
	l := postLinkFixture(t)
	if _, _, err := l.DeclareEntryLink("pay-1", "pay-2", LinkTypeAdjustmentOf); err != nil {
		t.Fatalf("declare: %v", err)
	}
	res, err := l.DryRunPost(JournalEntry{ID: "what-if", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 5, Currency: "USD"})
	if err != nil {
		t.Fatalf("DryRunPost: %v", err)
	}
	if !res.WouldSucceed {
		t.Errorf("DryRunPost = %+v, want would-succeed", res)
	}
	if n := len(l.entryLinks); n != 1 {
		t.Errorf("live entryLinks = %d after dry run, want 1", n)
	}
	if c := l.EntryLinkChangeCount(); c != 1 {
		t.Errorf("live EntryLinkChangeCount = %d after dry run, want 1", c)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after dry run: %v", err)
	}
}
