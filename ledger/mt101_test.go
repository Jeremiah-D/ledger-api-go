package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt101ExecuteOn is the fixed execution date the MT101 tests pin:
// 2026-11-12, so :30: is 261112 and the :20: derivation is
// deterministic.
var mt101ExecuteOn = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

func mt101Opts(debtor string) MT101ExportOptions {
	return MT101ExportOptions{
		Debtor:    AccountID(debtor),
		ExecuteOn: mt101ExecuteOn,
	}
}

// mt101Ref extracts the :20: reference from a rendered MT101.
func mt101Ref(t *testing.T, doc string) string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, ":20:") {
			return strings.TrimPrefix(line, ":20:")
		}
	}
	t.Fatalf("no :20: in export:\n%s", doc)
	return ""
}

func TestExportMT101ExactTags(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	mustPostTransferPain(t, l, "pay-2", "alice", "carol", 25050, "USD", base.Add(time.Hour))

	doc, err := l.ExportMT101(mt101Opts("alice"))
	if err != nil {
		t.Fatalf("ExportMT101: %v", err)
	}
	// Block framing: sender LT (debtor BIC, normalized), receiver
	// LT (debtor servicing-bank BIC — same BIC here), MUR = :20:.
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I101DEUTDEFFXXXU3003}",
		"{4:",
		":15A:",
		":28D:1/1",
		// :50H: is the debtor IBAN, account line only — no name
		// line, the ledger holds no legal holder name.
		":50H:/" + testIBANDE,
		":52A:" + testBICDE,
		":30:261112",
		":15B:",
		":21:pay-1",
		":32B:USD150,00",
		// bob has no registered BIC: :57D: with the To account ID.
		":57D:bob",
		":59:/" + testIBANGB,
		":71A:SHA",
		":15B:",
		":21:pay-2",
		":32B:USD250,50",
		// carol has a registered BIC: :57A:.
		":57A:" + testBICDE11,
		":59:/" + testIBANFR,
		"-}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No :70: (no memo data), no :33B:/:36: (no exchange leg), no
	// :50a: in sequence B (it would contradict :50H:).
	for _, forbidden := range []string{":70:", ":33B:", ":36:", ":50A:", ":50K:", ":23E:", ":77B:"} {
		if strings.Contains(doc, forbidden) {
			t.Errorf("export must not carry %s, body:\n%s", forbidden, doc)
		}
	}
	// :20: is exactly 16 characters: "MT101-" + 10 hex.
	ref := mt101Ref(t, doc)
	if len(ref) != 16 || !strings.HasPrefix(ref, "MT101-") {
		t.Errorf(":20: = %q, want MT101- + 10 hex (16 characters)", ref)
	}
	if strings.HasSuffix(doc, "\n") {
		t.Error("export must not end with a trailing newline")
	}
	// The same execution day re-renders byte-identical: the
	// reference derives from the content, not the clock.
	doc2, err := l.ExportMT101(mt101Opts("alice"))
	if err != nil {
		t.Fatalf("second ExportMT101: %v", err)
	}
	if doc2 != doc {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", doc, doc2)
	}
}

func TestExportMT101ChargeBearer(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)

	opts := mt101Opts("alice")
	opts.ChargeBearer = "OUR"
	doc, err := l.ExportMT101(opts)
	if err != nil {
		t.Fatalf("ExportMT101: %v", err)
	}
	if !strings.Contains(doc, ":71A:OUR") {
		t.Errorf("want :71A:OUR, body:\n%s", doc)
	}

	opts.ChargeBearer = "BOGUS"
	if _, err := l.ExportMT101(opts); !errors.Is(err, ErrInvalidMT101Export) {
		t.Errorf("bad charge bearer: want ErrInvalidMT101Export, got %v", err)
	}
}

func TestExportMT101ZeroPayments(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	// No EUR payments from alice: an MT101 with no sequence B is
	// not an instruction a bank can act on.
	opts := mt101Opts("alice")
	opts.Currency = "EUR"
	if _, err := l.ExportMT101(opts); !errors.Is(err, ErrInvalidMT101Export) {
		t.Errorf("zero payments: want ErrInvalidMT101Export, got %v", err)
	}
}

func TestExportMT101PendingReviewExcluded(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	mustPostTransferPain(t, l, "pay-2", "alice", "bob", 25000, "USD", base.Add(time.Hour))
	// Freeze pay-1 in dual-control review: its rows never touched
	// balances, so no instruction may be generated from it. Entries
	// are stored by value, so the mutated copy must be written
	// back into the map.
	if e, ok := l.entries["pay-1"]; !ok {
		t.Fatalf("no journal entry for pay-1")
	} else {
		e.PendingReview = true
		l.entries["pay-1"] = e
	}
	doc, err := l.ExportMT101(mt101Opts("alice"))
	if err != nil {
		t.Fatalf("ExportMT101: %v", err)
	}
	if strings.Contains(doc, ":21:pay-1") {
		t.Errorf("pending-review transfer must be excluded, body:\n%s", doc)
	}
	if !strings.Contains(doc, ":21:pay-2") {
		t.Errorf("approved transfer must still export, body:\n%s", doc)
	}
	if got := strings.Count(doc, ":15B:"); got != 1 {
		t.Errorf("sequence B blocks = %d, want 1", got)
	}
}

func TestExportMT101MissingDebtor(t *testing.T) {
	l := painTestLedger(t)
	if _, err := l.ExportMT101(MT101ExportOptions{ExecuteOn: mt101ExecuteOn}); !errors.Is(err, ErrInvalidMT101Export) {
		t.Errorf("missing debtor: want ErrInvalidMT101Export, got %v", err)
	}
}

func TestExportMT101InvalidCurrencyAndRange(t *testing.T) {
	l := painTestLedger(t)
	opts := mt101Opts("alice")
	opts.Currency = "USDD"
	if _, err := l.ExportMT101(opts); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("bad currency: want ErrInvalidCurrency, got %v", err)
	}
	opts = mt101Opts("alice")
	opts.Since = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	opts.Until = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if _, err := l.ExportMT101(opts); !errors.Is(err, ErrInvalidPain001Range) {
		t.Errorf("inverted range: want ErrInvalidPain001Range, got %v", err)
	}
}

func TestExportMT101BadTransferID(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	// A transfer ID over 16 characters cannot be a :21:.
	mustPostTransferPain(t, l, "bad id with spaces!", "alice", "bob", 15000, "USD", base)
	if _, err := l.ExportMT101(mt101Opts("alice")); !errors.Is(err, ErrInvalidMT101Export) {
		t.Errorf("over-long transfer ID: want ErrInvalidMT101Export, got %v", err)
	}
}

func TestExportMT101MissingIBAN(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	// dave has no registered IBAN: the export fails closed rather
	// than emitting a forged identifier.
	mustPostTransferPain(t, l, "pay-1", "alice", "dave", 15000, "USD", base)
	if _, err := l.ExportMT101(mt101Opts("alice")); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Errorf("missing creditor IBAN: want ErrMissingAccountIBAN, got %v", err)
	}
}

func TestExportMT101NoDebtorBIC(t *testing.T) {
	// Debtor and creditor without registered BICs: no :52A:, the
	// block 1/2 LTs derive from the debtor's LT-form account ID,
	// and the creditor renders :57D:. The account IDs are LT-form
	// (12 alphanumeric characters) on purpose: a non-LT-form
	// account ID would fail the header derivation.
	l := New()
	mustSetIBAN(t, l, "ACCTDEBTOR01", testIBANDE, "")
	mustSetIBAN(t, l, "ACCTCREDTR01", testIBANGB, "")
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "ACCTDEBTOR01", "ACCTCREDTR01", 15000, "USD", base)
	doc, err := l.ExportMT101(mt101Opts("ACCTDEBTOR01"))
	if err != nil {
		t.Fatalf("ExportMT101: %v", err)
	}
	for _, want := range []string{
		"{1:F01ACCTDEBTOR010000000000}",
		"{2:I101ACCTDEBTOR01U3003}",
		":50H:/" + testIBANDE,
		":57D:ACCTCREDTR01",
		":59:/" + testIBANGB,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, ":52A:") {
		t.Errorf("no-BIC debtor must not carry :52A:, body:\n%s", doc)
	}
}

func TestExportMT101AuditEventShape(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, testBICDE)
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "a1", "alice", "bob", 12000, "USD", time.Time{})
	mustPostTransferPain(t, l, "a2", "alice", "bob", 8000, "USD", time.Time{})

	vBefore := l.Version()
	doc, err := l.ExportMT101(mt101Opts("alice"))
	if err != nil {
		t.Fatalf("ExportMT101: %v", err)
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
	var found *AuditEvent
	for i, ev := range events {
		if ev.Op == "mt101_exported" {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no mt101_exported audit event; ops seen: %v", opNames(events))
	}
	if found.Actor != "ExportMT101" {
		t.Errorf("actor = %q, want ExportMT101", found.Actor)
	}
	if found.VersionBefore != vBefore || found.VersionAfter != vBefore {
		t.Errorf("version bracket = %d/%d, want %d/%d (read-only)", found.VersionBefore, found.VersionAfter, vBefore, vBefore)
	}
	if len(found.EntryIDs) != 0 {
		t.Errorf("entry_ids = %v, want empty (nothing booked)", found.EntryIDs)
	}
	d := found.Details
	if d["tx_count"] != float64(2) {
		t.Errorf("tx_count = %v, want 2", d["tx_count"])
	}
	if d["ctrl_sum_cents"] != float64(20000) {
		t.Errorf("ctrl_sum_cents = %v, want 20000", d["ctrl_sum_cents"])
	}
	if d["currency"] != "USD" {
		t.Errorf("currency = %v, want USD", d["currency"])
	}
	// The :20: in the document is the audit event's trace ID.
	if found.TraceID != mt101Ref(t, doc) {
		t.Errorf("trace_id = %q, want the :20: reference %q", found.TraceID, mt101Ref(t, doc))
	}
	// The export is read-only: no version bump, no chain link.
	if got := l.Version(); got != vBefore {
		t.Errorf("version moved %d -> %d on a read-only export", vBefore, got)
	}
}
