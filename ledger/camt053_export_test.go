package ledger

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestCAMT053ExportRenderRoundTrip is the core invertibility test:
// RenderCAMT053Statement must emit exactly what ParseCAMT053 reads
// back — import(export(x)) == x.
func TestCAMT053ExportRenderRoundTrip(t *testing.T) {
	base := time.Date(2026, 10, 20, 9, 30, 15, 0, time.UTC)
	txns := []CAMT053Transaction{
		{Account: "DE89370400440532013000", Currency: "EUR", StmtID: "STMT-1", Seq: 1, Direction: CAMT053DirectionIn, AmountCents: 25000, PostedAt: base, Memo: "invoice 42", EndToEndID: "e2e-1"},
		{Account: "DE89370400440532013000", Currency: "EUR", StmtID: "STMT-1", Seq: 2, Direction: CAMT053DirectionOut, AmountCents: 999, PostedAt: base.Add(2 * time.Hour), EndToEndID: "e2e-2"},
		{Account: "DE89370400440532013000", Currency: "USD", StmtID: "STMT-1", Seq: 3, Direction: CAMT053DirectionIn, AmountCents: 1, PostedAt: base.Add(26 * time.Hour), Memo: "a | b", EndToEndID: "e2e-3"},
	}
	doc, err := RenderCAMT053Statement("DE89370400440532013000", "STMT-1", "MSG-1", txns)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), `xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"`) {
		t.Fatal("export does not declare the camt.053.001.02 namespace")
	}
	if !strings.HasPrefix(string(doc), "<?xml") {
		t.Fatal("export does not start with the XML declaration")
	}
	back, err := ParseCAMT053(doc)
	if err != nil {
		t.Fatalf("re-parse of own export: %v", err)
	}
	if !reflect.DeepEqual(back, txns) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, txns)
	}
	// MsgId default: empty msgID falls back to the statement ID.
	doc2, err := RenderCAMT053Statement("acct", "STMT-9", "", txns[:1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc2), "<MsgId>STMT-9</MsgId>") {
		t.Fatalf("default MsgId not applied:\n%s", doc2)
	}
}

func TestCAMT053ExportFromLedger(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	acct := AccountID("ops")
	// ops is the debit leg -> inflow (CRDT).
	mustPostOFX(t, l, "x1", acct, "equity", 15000, "USD", base)
	// ops is the credit leg -> outflow (DBIT), with an idempotency key.
	e2, dup, err := l.Post(JournalEntry{ID: "x2", DebitAccount: "rent", CreditAccount: acct, AmountCents: 20000, Currency: "USD", CreatedAt: base.Add(time.Hour), Memo: "rent oct", IdempotencyKey: "key-x2"})
	if err != nil || dup {
		t.Fatalf("Post(x2): %v dup=%v", err, dup)
	}
	_ = e2
	// Different currency, same account.
	mustPostOFX(t, l, "x3", acct, "equity", 5000, "EUR", base.Add(2*time.Hour))

	doc, err := l.ExportCAMT053(CAMT053ExportOptions{Account: acct, StmtID: "S-1"})
	if err != nil {
		t.Fatal(err)
	}
	txns, err := ParseCAMT053(doc)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(txns) != 3 {
		t.Fatalf("got %d transactions, want 3", len(txns))
	}
	want := []struct {
		dir    CAMT053Direction
		amount int64
		ccy    string
		e2e    string
		memo   string
		seq    int
	}{
		{CAMT053DirectionIn, 15000, "USD", "x1", "", 1},
		{CAMT053DirectionOut, 20000, "USD", "key-x2", "rent oct", 2},
		{CAMT053DirectionIn, 5000, "EUR", "x3", "", 3},
	}
	for i, w := range want {
		g := txns[i]
		if g.Direction != w.dir || g.AmountCents != w.amount || g.Currency != w.ccy ||
			g.EndToEndID != w.e2e || g.Memo != w.memo || g.Seq != w.seq ||
			g.StmtID != "S-1" || g.Account != acct {
			t.Fatalf("txn %d: got %+v, want %+v", i, g, w)
		}
	}
	if !txns[0].PostedAt.Equal(base) {
		t.Fatalf("booking date lost precision: %v", txns[0].PostedAt)
	}
	// Currency filter.
	docUSD, err := l.ExportCAMT053(CAMT053ExportOptions{Account: acct, StmtID: "S-2", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	txnsUSD, err := ParseCAMT053(docUSD)
	if err != nil {
		t.Fatal(err)
	}
	if len(txnsUSD) != 2 {
		t.Fatalf("currency filter: got %d, want 2", len(txnsUSD))
	}
	// Window filter: only x2 falls in [base+30m, base+90m).
	docW, err := l.ExportCAMT053(CAMT053ExportOptions{Account: acct, StmtID: "S-3", Since: base.Add(30 * time.Minute), Until: base.Add(90 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	txnsW, err := ParseCAMT053(docW)
	if err != nil {
		t.Fatal(err)
	}
	if len(txnsW) != 1 || txnsW[0].EndToEndID != "key-x2" {
		t.Fatalf("window filter: %+v", txnsW)
	}
}

func TestCAMT053ExportValidation(t *testing.T) {
	l := New()
	if _, err := l.ExportCAMT053(CAMT053ExportOptions{StmtID: "S"}); err == nil {
		t.Fatal("expected error for empty account")
	}
	if _, err := l.ExportCAMT053(CAMT053ExportOptions{Account: "a"}); err == nil {
		t.Fatal("expected error for empty statement ID")
	}
	if _, err := l.ExportCAMT053(CAMT053ExportOptions{Account: "a", StmtID: "S", Currency: "US"}); err == nil {
		t.Fatal("expected error for bad currency")
	}
	base := time.Now().UTC()
	if _, err := l.ExportCAMT053(CAMT053ExportOptions{Account: "a", StmtID: "S", Since: base, Until: base.Add(-time.Hour)}); err == nil {
		t.Fatal("expected error for inverted range")
	}
	if _, err := RenderCAMT053Statement("a", "", "m", nil); err == nil {
		t.Fatal("expected error for empty statement ID in render")
	}
	if _, err := RenderCAMT053Statement("", "s", "m", nil); err == nil {
		t.Fatal("expected error for empty account in render")
	}
	badDir := []CAMT053Transaction{{Account: "a", Currency: "USD", StmtID: "s", Seq: 1, Direction: "sideways", AmountCents: 1, PostedAt: base}}
	if _, err := RenderCAMT053Statement("a", "s", "m", badDir); err == nil {
		t.Fatal("expected error for bad direction in render")
	}
	badCcy := []CAMT053Transaction{{Account: "a", Currency: "usd", StmtID: "s", Seq: 1, Direction: CAMT053DirectionIn, AmountCents: 1, PostedAt: base}}
	if _, err := RenderCAMT053Statement("a", "s", "m", badCcy); err == nil {
		t.Fatal("expected error for lowercase currency in render")
	}
	noDate := []CAMT053Transaction{{Account: "a", Currency: "USD", StmtID: "s", Seq: 1, Direction: CAMT053DirectionIn, AmountCents: 1}}
	if _, err := RenderCAMT053Statement("a", "s", "m", noDate); err == nil {
		t.Fatal("expected error for missing booking date in render")
	}
}

func TestCAMT053ExportEmpty(t *testing.T) {
	l := New()
	doc, err := l.ExportCAMT053(CAMT053ExportOptions{Account: "ghost", StmtID: "S-EMPTY"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if !strings.Contains(s, "<Id>S-EMPTY</Id>") {
		t.Fatalf("empty export missing Stmt Id:\n%s", s)
	}
	if strings.Contains(s, "<Ntry>") {
		t.Fatalf("empty export should have no entries:\n%s", s)
	}
	// Documented asymmetry: the parser is strict about importing only
	// real statements, so it rejects the empty export.
	if _, err := ParseCAMT053(doc); err == nil {
		t.Fatal("expected ParseCAMT053 to reject the empty export")
	}
}

func TestCAMT053ExportExternalMapping(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "m1", "ops", "equity", 1000, "USD", base)
	if err := l.SetExternalMapping("ops", ExternalSystemCAMT053, "DE89370400440532013000", "test"); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportCAMT053(CAMT053ExportOptions{Account: "ops", StmtID: "S-M"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "DE89370400440532013000</Id>") {
		t.Fatalf("export does not name the mapped account ID:\n%s", doc)
	}
	txns, err := ParseCAMT053(doc)
	if err != nil {
		t.Fatal(err)
	}
	if txns[0].Account != "DE89370400440532013000" {
		t.Fatalf("parsed account: %q", txns[0].Account)
	}
}

// TestCAMT053ExportSkipsPendingReview pins the OFX/statement rule: a
// pending-review row never touched balances, so the bank statement
// must not show it.
func TestCAMT053ExportSkipsPendingReview(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "v1", "ops", "equity", 1000, "USD", base)
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "ops", CreditAccount: "equity", AmountCents: 2000, Currency: "USD", CreatedAt: base, PendingReview: true}); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportCAMT053(CAMT053ExportOptions{Account: "ops", StmtID: "S-P"})
	if err != nil {
		t.Fatal(err)
	}
	txns, err := ParseCAMT053(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 1 || txns[0].AmountCents != 1000 {
		t.Fatalf("pending-review row leaked into export: %+v", txns)
	}
}
