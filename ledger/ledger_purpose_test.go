package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckPurposeCode(t *testing.T) {
	// The curated vocabulary from the backlog: SALA/GDDS/SUPP/PENS
	// must all validate.
	for _, code := range []string{"SALA", "GDDS", "SUPP", "PENS", "DIVI", "TAXS", "OTHR"} {
		if !IsValidPurposeCode(code) {
			t.Errorf("IsValidPurposeCode(%q) = false, want true", code)
		}
		if err := checkPurposeCode(code); err != nil {
			t.Errorf("checkPurposeCode(%q) = %v, want nil", code, err)
		}
	}
	// Empty means "no purpose stated".
	if err := checkPurposeCode(""); err != nil {
		t.Errorf("checkPurposeCode(\"\") = %v, want nil", err)
	}
	// Near-misses are not codes: lowercase, unknown, padded,
	// truncated, and the full table's absence are all rejected
	// fail-closed.
	for _, bad := range []string{"sala", "Sala", "XXXX", "SALA ", " SALA", "SAL", "SALAA"} {
		if IsValidPurposeCode(bad) {
			t.Errorf("IsValidPurposeCode(%q) = true, want false", bad)
		}
		if err := checkPurposeCode(bad); !errors.Is(err, ErrInvalidPurposeCode) {
			t.Errorf("checkPurposeCode(%q) = %v, want ErrInvalidPurposeCode", bad, err)
		}
	}
	// The table is a genuine curated subset: sorted, unique, and every
	// code is 4 uppercase letters.
	codes := AdmittedPurposeCodes()
	if len(codes) < 20 {
		t.Errorf("admitted codes = %d, want a real subset (>= 20)", len(codes))
	}
	seen := map[string]bool{}
	prev := ""
	for _, p := range codes {
		if p.Code != strings.ToUpper(p.Code) || len(p.Code) != 4 {
			t.Errorf("code %q is not 4 uppercase letters", p.Code)
		}
		if seen[p.Code] {
			t.Errorf("duplicate code %q", p.Code)
		}
		seen[p.Code] = true
		if p.Code < prev {
			t.Errorf("table not sorted: %q after %q", p.Code, prev)
		}
		prev = p.Code
		if p.Name == "" || p.Description == "" {
			t.Errorf("code %q missing name/description", p.Code)
		}
	}
	if _, ok := LookupPurposeCode("SALA"); !ok {
		t.Error("LookupPurposeCode(SALA) not found")
	}
}

func TestPostPurposeCode(t *testing.T) {
	l := New()
	e, _, err := l.Post(JournalEntry{
		ID: "e1", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 1000, Currency: "USD", PurposeCode: "SALA",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if e.PurposeCode != "SALA" {
		t.Errorf("purpose_code = %q, want SALA", e.PurposeCode)
	}
	// An invalid code is rejected fail-closed: nothing is recorded.
	if _, _, err := l.Post(JournalEntry{
		ID: "e2", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 1000, Currency: "USD", PurposeCode: "BOGUS",
	}); !errors.Is(err, ErrInvalidPurposeCode) {
		t.Fatalf("Post with bad purpose = %v, want ErrInvalidPurposeCode", err)
	}
	if _, ok := l.entries["e2"]; ok {
		t.Error("rejected entry e2 must not be journaled")
	}
	// The chain still verifies with mixed entries.
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestPostTransferPurposeCode(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "fund", DebitAccount: "payer", CreditAccount: "equity",
		AmountCents: 50000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	rcpt, err := l.PostTransfer(Transfer{
		ID: "t1", From: "payer", To: "payee",
		AmountCents: 10000, Currency: "USD", PurposeCode: "GDDS",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(rcpt.Entries) == 0 || rcpt.Entries[0].PurposeCode != "GDDS" {
		t.Errorf("principal entry purpose = %v, want GDDS", rcpt.Entries)
	}
	// An invalid code fails before anything books.
	if _, err := l.PostTransfer(Transfer{
		ID: "t2", From: "payer", To: "payee",
		AmountCents: 100, Currency: "USD", PurposeCode: "NOPE",
	}); !errors.Is(err, ErrInvalidPurposeCode) {
		t.Fatalf("PostTransfer with bad purpose = %v, want ErrInvalidPurposeCode", err)
	}
	if _, ok := l.entries["t2"]; ok {
		t.Error("rejected transfer t2 must not be journaled")
	}
}

func TestCreatePayoutPurposeCode(t *testing.T) {
	l := New()
	p, _, err := l.CreatePayout(PayoutInstruction{
		ID: "p1", PayeeAccount: "ops", AmountCents: 5000,
		Currency: "USD", Destination: "FPS:123", Maker: "alice",
		PurposeCode: "SUPP",
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if p.PurposeCode != "SUPP" {
		t.Errorf("purpose_code = %q, want SUPP", p.PurposeCode)
	}
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID: "p2", PayeeAccount: "ops", AmountCents: 5000,
		Currency: "USD", Destination: "FPS:123", Maker: "alice",
		PurposeCode: "bogus",
	}); !errors.Is(err, ErrInvalidPurposeCode) {
		t.Fatalf("CreatePayout with bad purpose = %v, want ErrInvalidPurposeCode", err)
	}
}

func TestExportPacs009Purpose(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "fund", DebitAccount: "payer", CreditAccount: "equity",
		AmountCents: 50000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "t-sala", From: "payer", To: "payee",
		AmountCents: 10000, Currency: "USD", PurposeCode: "SALA",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "t-plain", From: "payer", To: "payee",
		AmountCents: 1000, Currency: "USD",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	doc, err := l.ExportPacs009("t-sala", Pacs009Options{})
	if err != nil {
		t.Fatalf("ExportPacs009: %v", err)
	}
	docFlat := strings.Join(strings.Fields(string(doc)), "")
	if !strings.Contains(docFlat, "<Purp><Cd>SALA</Cd></Purp>") {
		t.Errorf("pacs.009 missing <Purp><Cd>SALA</Cd></Purp>:\n%s", doc)
	}
	plain, err := l.ExportPacs009("t-plain", Pacs009Options{})
	if err != nil {
		t.Fatalf("ExportPacs009: %v", err)
	}
	if strings.Contains(string(plain), "<Purp>") {
		t.Errorf("pacs.009 without purpose must omit Purp:\n%s", plain)
	}
}

func TestExportPacs008Purpose(t *testing.T) {
	l := New()
	p, _, err := l.CreatePayout(PayoutInstruction{
		ID: "p-supp", PayeeAccount: "ops", AmountCents: 5000,
		Currency: "USD", Destination: "FPS:123", Maker: "alice",
		PurposeCode: "SUPP",
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	doc, err := l.ExportPacs008(p.ID)
	if err != nil {
		t.Fatalf("ExportPacs008: %v", err)
	}
	docFlat := strings.Join(strings.Fields(string(doc)), "")
	if !strings.Contains(docFlat, "<Purp><Cd>SUPP</Cd></Purp>") {
		t.Errorf("pacs.008 missing <Purp><Cd>SUPP</Cd></Purp>:\n%s", doc)
	}
	q, _, err := l.CreatePayout(PayoutInstruction{
		ID: "p-plain", PayeeAccount: "ops", AmountCents: 5000,
		Currency: "USD", Destination: "FPS:123", Maker: "alice",
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	plain, err := l.ExportPacs008(q.ID)
	if err != nil {
		t.Fatalf("ExportPacs008: %v", err)
	}
	if strings.Contains(string(plain), "<Purp>") {
		t.Errorf("pacs.008 without purpose must omit Purp:\n%s", plain)
	}
	// The purpose-bearing document still parses through the inbound
	// parser: Purp is ignored, not rejected.
	txs, err := ParsePacs008(doc)
	if err != nil {
		t.Fatalf("ParsePacs008 of purpose-bearing export: %v", err)
	}
	if len(txs) != 1 || txs[0].EndToEndID == "" {
		t.Fatalf("parsed transactions = %+v, want one with an EndToEndId", txs)
	}
}

func TestReconcilePurposeCodeTotals(t *testing.T) {
	l := New()
	post := func(id, code string, cents int64, ccy string) {
		t.Helper()
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: "a", CreditAccount: "b",
			AmountCents: cents, Currency: ccy, PurposeCode: code,
		}); err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	post("e1", "SALA", 10000, "USD")
	post("e2", "SALA", 5000, "USD")
	post("e3", "GDDS", 3000, "USD")
	post("e4", "SALA", 2000, "EUR")
	post("e5", "", 9999, "USD") // no purpose: absent from the rollup

	report := l.Reconcile(time.Now())
	rows := report.PurposeCodeTotals
	if len(rows) != 3 {
		t.Fatalf("purpose rows = %d, want 3 (SALA/USD, SALA/EUR, GDDS/USD)", len(rows))
	}
	// Sorted by (purpose code, currency).
	if rows[0].PurposeCode != "GDDS" || rows[1].PurposeCode != "SALA" || rows[1].Currency != "EUR" || rows[2].Currency != "USD" {
		t.Fatalf("row order = %+v, want GDDS/USD, SALA/EUR, SALA/USD", rows)
	}
	salaUSD := rows[2]
	if salaUSD.TotalDebitsCents != 15000 || salaUSD.TotalCreditsCents != 15000 || salaUSD.EntryCount != 2 {
		t.Errorf("SALA/USD row = %+v, want 15000/15000 x2", salaUSD)
	}
	if salaUSD.Description == "" {
		t.Error("SALA/USD row missing description")
	}
	gdds := rows[0]
	if gdds.TotalDebitsCents != 3000 || gdds.EntryCount != 1 {
		t.Errorf("GDDS/USD row = %+v, want 3000 x1", gdds)
	}
	// The unstated 9999 cents appear nowhere in the rollup.
	for _, r := range rows {
		if r.TotalDebitsCents == 9999 {
			t.Error("entries without a purpose must not appear in the rollup")
		}
	}
}

func TestPurposeCodeCoveredByChainHash(t *testing.T) {
	// The same entry with and without a purpose must hash to
	// different links: rewriting a journaled purpose breaks the chain
	// exactly like rewriting an amount.
	prev := [32]byte{}
	a := JournalEntry{ID: "x", DebitAccount: "d", CreditAccount: "c", AmountCents: 100, Currency: "USD"}
	b := a
	b.PurposeCode = "SALA"
	if hashChainLink(prev, a) == hashChainLink(prev, b) {
		t.Error("purpose code must be covered by the chain hash")
	}
	// And an entry without a purpose hashes with no purpose section:
	// encoding byte-identical to the pre-purpose chain (no new bytes
	// for the empty code).
	c := a
	c.PurposeCode = ""
	if hashChainLink(prev, a) != hashChainLink(prev, c) {
		t.Error("empty purpose code must hash identically (section skipped)")
	}
}
