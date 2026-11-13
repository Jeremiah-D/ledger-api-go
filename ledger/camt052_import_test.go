package ledger

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// camt052Doc renders a realistic camt.052.001.02 intraday account
// report document for the given Rpt Id and account (IBAN: prefix
// means the IBAN shape, else Other/Id) with the given Bal blocks and
// Ntry bodies. It carries the official default namespace so the test
// proves the namespace, and includes the GrpHdr a real bank report
// carries.
func camt052Doc(rptID, acct string, bals []string, entries ...string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.052.001.02">` + "\n")
	sb.WriteString("  <BkToCstmrAcctRpt>\n    <GrpHdr>\n      <MsgId>MSG-20261016-052</MsgId>\n      <CreDtTm>2026-10-16T08:00:00Z</CreDtTm>\n    </GrpHdr>\n    <Rpt>\n")
	sb.WriteString("      <Id>" + rptID + "</Id>\n")
	if strings.HasPrefix(acct, "IBAN:") {
		sb.WriteString("      <Acct>\n        <Id>\n          <IBAN>" + strings.TrimPrefix(acct, "IBAN:") + "</IBAN>\n        </Id>\n        <Ccy>EUR</Ccy>\n      </Acct>\n")
	} else {
		sb.WriteString("      <Acct>\n        <Id>\n          <Othr>\n            <Id>" + acct + "</Id>\n          </Othr>\n        </Id>\n        <Ccy>USD</Ccy>\n      </Acct>\n")
	}
	for _, b := range bals {
		sb.WriteString(b)
	}
	for _, e := range entries {
		sb.WriteString("      <Ntry>\n" + e + "      </Ntry>\n")
	}
	sb.WriteString("    </Rpt>\n  </BkToCstmrAcctRpt>\n</Document>\n")
	return sb.String()
}

// camt052Bal renders one Bal block with a date-only Dt.
func camt052Bal(typ, amt, ccy, ind, dt string) string {
	return "      <Bal>\n" +
		"        <Tp><CdOrPrtry><Cd>" + typ + "</Cd></CdOrPrtry></Tp>\n" +
		"        <Amt Ccy=\"" + ccy + "\">" + amt + "</Amt>\n" +
		"        <CdtDbtInd>" + ind + "</CdtDbtInd>\n" +
		"        <Dt><Dt>" + dt + "</Dt></Dt>\n" +
		"      </Bal>\n"
}

// camt052BalDtTm renders one Bal block with a datetime DtTm.
func camt052BalDtTm(typ, amt, ccy, ind, dtTm string) string {
	return "      <Bal>\n" +
		"        <Tp><CdOrPrtry><Cd>" + typ + "</Cd></CdOrPrtry></Tp>\n" +
		"        <Amt Ccy=\"" + ccy + "\">" + amt + "</Amt>\n" +
		"        <CdtDbtInd>" + ind + "</CdtDbtInd>\n" +
		"        <Dt><DtTm>" + dtTm + "</DtTm></Dt>\n" +
		"      </Bal>\n"
}

func TestParseCAMT052SampleVector(t *testing.T) {
	doc := camt052Doc("RPT-2026-10-16", "IBAN:DE89370400440532013000",
		[]string{
			camt052Bal("OPBD", "1000.00", "EUR", "CRDT", "2026-10-05"),
			camt052Bal("CLBD", "1207.50", "EUR", "CRDT", "2026-10-06"),
			camt052Bal("CLBD", "50.25", "USD", "DBIT", "2026-10-06"),
		},
		camt053NtryIn, camt053NtryOut)
	stmt, err := ParseCAMT052([]byte(doc))
	if err != nil {
		t.Fatalf("ParseCAMT052: %v", err)
	}
	if len(stmt.Balances) != 3 {
		t.Fatalf("parsed %d balances, want 3", len(stmt.Balances))
	}
	opbd := stmt.Balances[0]
	if opbd.Type != CAMT052BalanceOPBD || opbd.Currency != "EUR" || opbd.AmountCents != 100000 {
		t.Errorf("OPBD = %+v, want OPBD/EUR/100000", opbd)
	}
	if want := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC); !opbd.At.Equal(want) {
		t.Errorf("OPBD At = %v, want %v", opbd.At, want)
	}
	clbd := stmt.Balances[1]
	if clbd.Type != CAMT052BalanceCLBD || clbd.Currency != "EUR" || clbd.AmountCents != 120750 {
		t.Errorf("CLBD = %+v, want CLBD/EUR/120750", clbd)
	}
	// A DBIT balance is a negative (debit) balance.
	usd := stmt.Balances[2]
	if usd.Type != CAMT052BalanceCLBD || usd.Currency != "USD" || usd.AmountCents != -5025 {
		t.Errorf("USD CLBD = %+v, want CLBD/USD/-5025", usd)
	}
	if len(stmt.Transactions) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(stmt.Transactions))
	}
	in := stmt.Transactions[0]
	if in.Account != "DE89370400440532013000" || in.Currency != "EUR" || in.StmtID != "RPT-2026-10-16" || in.Seq != 1 {
		t.Errorf("in entry identity = %+v", in)
	}
	if in.Direction != CAMT053DirectionIn || in.AmountCents != 25000 {
		t.Errorf("in entry = dir %q amount %d, want in/25000", in.Direction, in.AmountCents)
	}
	if in.Memo != "Invoice 1042 | October services" || in.EndToEndID != "E2E-INV-1042" {
		t.Errorf("in memo/e2e = %q/%q", in.Memo, in.EndToEndID)
	}
	out := stmt.Transactions[1]
	if out.Direction != CAMT053DirectionOut || out.AmountCents != 4250 {
		t.Errorf("out entry = dir %q amount %d, want out/4250", out.Direction, out.AmountCents)
	}
	// The +02:00 datetime normalizes to UTC.
	if want := time.Date(2026, 10, 15, 12, 30, 0, 0, time.UTC); !out.PostedAt.Equal(want) {
		t.Errorf("out PostedAt = %v, want %v", out.PostedAt, want)
	}
}

func TestParseCAMT052OtherAccountID(t *testing.T) {
	doc := camt052Doc("RPT-9", "cash",
		[]string{camt052Bal("OPBD", "0.00", "USD", "CRDT", "2026-10-05")},
		camt053NtryIn)
	stmt, err := ParseCAMT052([]byte(doc))
	if err != nil {
		t.Fatalf("ParseCAMT052: %v", err)
	}
	if len(stmt.Transactions) != 1 || stmt.Transactions[0].Account != "cash" {
		t.Fatalf("account = %+v, want cash", stmt.Transactions)
	}
}

func TestParseCAMT052Rejects(t *testing.T) {
	good := camt052Doc("RPT-1", "IBAN:DE89370400440532013000",
		[]string{camt052Bal("OPBD", "1000.00", "EUR", "CRDT", "2026-10-05")},
		camt053NtryIn)
	cases := []struct {
		name string
		mut  func(string) string
	}{
		{"empty", func(string) string { return "  \n" }},
		{"not XML", func(string) string { return "<Document><broken" }},
		{"no Rpt", func(s string) string { return strings.Replace(s, "<Rpt>", "<Nope>", 1) }},
		{"Rpt without Id", func(s string) string { return strings.Replace(s, "<Id>RPT-1</Id>", "<Id></Id>", 1) }},
		{"bad Bal type", func(s string) string {
			return strings.Replace(s, "<Cd>OPBD</Cd>", "<Cd>ITBD</Cd>", 1)
		}},
		{"bad Bal amount", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">1000.00</Amt>`, `<Amt Ccy="EUR">abc</Amt>`, 1)
		}},
		{"missing Bal Ccy", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">1000.00</Amt>`, `<Amt>1000.00</Amt>`, 1)
		}},
		{"bad Bal Ccy", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">1000.00</Amt>`, `<Amt Ccy="EURO">1000.00</Amt>`, 1)
		}},
		{"missing Bal date", func(s string) string {
			return strings.Replace(s, "<Dt><Dt>2026-10-05</Dt></Dt>", "", 1)
		}},
		{"missing Ntry Amt", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">250.00</Amt>`, "", 1)
		}},
		{"bad Ntry Amt", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">250.00</Amt>`, `<Amt Ccy="EUR">abc</Amt>`, 1)
		}},
		{"missing booking date", func(s string) string {
			s = strings.Replace(s, "<BookgDt><Dt>2026-10-15</Dt></BookgDt>", "", 1)
			return strings.Replace(s, "<ValDt><Dt>2026-10-15</Dt></ValDt>", "", 1)
		}},
		{"zero Ntry", func(s string) string {
			return camt052Doc("RPT-1", "IBAN:DE89370400440532013000",
				[]string{camt052Bal("OPBD", "1000.00", "EUR", "CRDT", "2026-10-05")})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCAMT052([]byte(tc.mut(good))); !errors.Is(err, ErrInvalidCAMT052) {
				t.Fatalf("err = %v, want ErrInvalidCAMT052", err)
			}
		})
	}
}

func TestImportCAMT052MatchStates(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	// One unique entry -> matched.
	mustPostOFX(t, l, "m-1", acct, "equity", 25000, "EUR", base)
	// Two entries with the same |amount| inside the window -> mismatched.
	mustPostOFX(t, l, "m-2a", acct, "equity", 7777, "EUR", base.Add(-time.Hour))
	mustPostOFX(t, l, "m-2b", acct, "equity", 7777, "EUR", base.Add(time.Hour))
	// An entry outside the default 24h window must not match.
	mustPostOFX(t, l, "m-far", acct, "equity", 4242, "EUR", base.Add(-72*time.Hour))
	// Same amount, different currency: isolated, must not match.
	mustPostOFX(t, l, "m-usd", acct, "equity", 999, "USD", base)

	ntry := func(amt, ccy, cdtDbtInd, dt string) string {
		return "        <Amt Ccy=\"" + ccy + "\">" + amt + "</Amt>\n" +
			"        <CdtDbtInd>" + cdtDbtInd + "</CdtDbtInd>\n" +
			"        <BookgDt><Dt>" + dt + "</Dt></BookgDt>\n"
	}
	day := base.Format("2006-01-02")
	// OPBD at midnight: only the far entry (4242) predates it.
	// CLBD at 14:00: all four EUR rows fold in (447.96); the USD row
	// folds to 9.99. Both agree with the ledger: no mismatches.
	doc := camt052Doc("RPT-M", "IBAN:DE89370400440532013000",
		[]string{
			camt052Bal("OPBD", "42.42", "EUR", "CRDT", day),
			camt052BalDtTm("CLBD", "447.96", "EUR", "CRDT", "2026-10-05T14:00:00Z"),
			camt052BalDtTm("CLBD", "9.99", "USD", "CRDT", "2026-10-05T14:00:00Z"),
		},
		ntry("250.00", "EUR", "CRDT", day),
		ntry("77.77", "EUR", "CRDT", day),
		ntry("42.42", "EUR", "CRDT", day),
		ntry("9.99", "EUR", "DBIT", day),
	)
	_, linksBefore := l.ChainHead()
	versionBefore := l.Version()
	rep, err := l.ImportCAMT052(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT052: %v", err)
	}
	if rep.Matched != 1 || rep.Mismatched != 1 || rep.Unmatched != 2 {
		t.Fatalf("summary = %+v, want 1/1/2 matched/mismatched/unmatched", rep)
	}
	if rep.Duplicate {
		t.Error("first import must not be a duplicate")
	}
	if len(rep.Balances) != 3 {
		t.Fatalf("balances = %d, want 3", len(rep.Balances))
	}
	if b := rep.Balances[0]; b.Type != CAMT052BalanceOPBD || b.Currency != "EUR" || b.AmountCents != 4242 {
		t.Errorf("balances[0] = %+v, want OPBD/EUR/4242", b)
	}
	if b := rep.Balances[1]; b.Type != CAMT052BalanceCLBD || b.Currency != "EUR" || b.AmountCents != 44796 {
		t.Errorf("balances[1] = %+v, want CLBD/EUR/44796", b)
	}
	if len(rep.BalanceMismatches) != 0 {
		t.Errorf("balance mismatches = %+v, want none (CLBD agrees with the ledger)", rep.BalanceMismatches)
	}
	bySeq := map[int]CAMT053ImportItem{}
	for _, it := range rep.Items {
		bySeq[it.Transaction.Seq] = it
	}
	if it := bySeq[1]; it.Status != CAMT053Matched || it.MatchedEntryID != "m-1" {
		t.Errorf("seq 1 = %+v, want matched to m-1", it)
	}
	if it := bySeq[2]; it.Status != CAMT053Mismatched || it.CandidateCount != 2 {
		t.Errorf("seq 2 = %+v, want mismatched with 2 candidates", it)
	}
	if it := bySeq[3]; it.Status != CAMT053Unmatched {
		t.Errorf("seq 3 = %+v, want unmatched (only candidate outside window)", it)
	}
	if it := bySeq[4]; it.Status != CAMT053Unmatched {
		t.Errorf("seq 4 = %+v, want unmatched (currency isolation)", it)
	}
	// The import is a draft: no journal rows, no chain links, no
	// version movement.
	if _, linksAfter := l.ChainHead(); linksAfter != linksBefore {
		t.Errorf("chain links moved %d -> %d on import", linksBefore, linksAfter)
	}
	if v := l.Version(); v != versionBefore {
		t.Errorf("version moved %d -> %d on import", versionBefore, v)
	}
}

func TestImportCAMT052BalanceMismatch(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	mustPostOFX(t, l, "b-1", acct, "equity", 10000, "EUR", base)
	day := base.Format("2006-01-02")
	// The CLBD claims 999.99 but the ledger folds to 100.00 — the
	// import must still succeed and report the gap. The OPBD is
	// deliberately wrong too (55.55 vs the true 0.00): only CLBD is
	// compared, so it must not appear in the mismatches.
	doc := camt052Doc("RPT-B", "IBAN:DE89370400440532013000",
		[]string{
			camt052Bal("OPBD", "55.55", "EUR", "CRDT", day),
			camt052BalDtTm("CLBD", "999.99", "EUR", "CRDT", "2026-10-05T14:00:00Z"),
		},
		"        <Amt Ccy=\"EUR\">100.00</Amt>\n        <CdtDbtInd>CRDT</CdtDbtInd>\n        <BookgDt><Dt>"+day+"</Dt></BookgDt>\n")
	rep, err := l.ImportCAMT052(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT052: %v", err)
	}
	if rep.Matched != 1 {
		t.Fatalf("matched = %d, want 1", rep.Matched)
	}
	if len(rep.BalanceMismatches) != 1 {
		t.Fatalf("balance mismatches = %+v, want exactly the CLBD gap", rep.BalanceMismatches)
	}
	m := rep.BalanceMismatches[0]
	if m.Currency != "EUR" || m.ReportedCents != 99999 || m.LedgerCents != 10000 {
		t.Errorf("mismatch = %+v, want EUR reported 99999 ledger 10000", m)
	}
	if want := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC); !m.At.Equal(want) {
		t.Errorf("mismatch At = %v, want %v", m.At, want)
	}
}

func TestImportCAMT052Duplicate(t *testing.T) {
	l := New()
	acct := AccountID("DE89370400440532013000")
	doc := camt052Doc("RPT-D", "IBAN:DE89370400440532013000",
		[]string{camt052Bal("OPBD", "0.00", "EUR", "CRDT", "2026-10-05")},
		camt053NtryIn)
	first, err := l.ImportCAMT052(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("first ImportCAMT052: %v", err)
	}
	if first.DocumentSHA256 == "" {
		t.Fatal("report has no document SHA-256")
	}
	second, err := l.ImportCAMT052(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("second ImportCAMT052: %v", err)
	}
	if !second.Duplicate {
		t.Error("re-import must be marked duplicate")
	}
	if !reflect.DeepEqual(first.Items, second.Items) || !reflect.DeepEqual(first.Balances, second.Balances) ||
		first.Matched != second.Matched {
		t.Error("duplicate report must replay the original report")
	}
}

func TestImportCAMT052AccountMismatch(t *testing.T) {
	l := New()
	doc := camt052Doc("RPT-X", "IBAN:DE89370400440532013000",
		[]string{camt052Bal("OPBD", "0.00", "EUR", "CRDT", "2026-10-05")},
		camt053NtryIn)
	if _, err := l.ImportCAMT052("cash", []byte(doc), CAMT053ImportOptions{}); !errors.Is(err, ErrInvalidCAMT052) {
		t.Fatalf("err = %v, want ErrInvalidCAMT052", err)
	}
}

func TestImportCAMT052CustomWindow(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	mustPostOFX(t, l, "w-1", acct, "equity", 4242, "EUR", base.Add(-72*time.Hour))
	doc := camt052Doc("RPT-W", "IBAN:DE89370400440532013000",
		[]string{camt052Bal("OPBD", "0.00", "EUR", "CRDT", "2026-10-02")},
		"        <Amt Ccy=\"EUR\">42.42</Amt>\n        <CdtDbtInd>CRDT</CdtDbtInd>\n        <BookgDt><Dt>"+base.Format("2006-01-02")+"</Dt></BookgDt>\n")
	rep, err := l.ImportCAMT052(acct, []byte(doc), CAMT053ImportOptions{MatchWindow: 96 * time.Hour})
	if err != nil {
		t.Fatalf("ImportCAMT052: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedEntryID != "w-1" {
		t.Fatalf("custom window should match: %+v", rep.Items[0])
	}
}

func TestImportCAMT052RoundTrip(t *testing.T) {
	// ParseCAMT052 is the inverse of RenderCAMT052Report: export a
	// report from the journal, then import the exact bytes back —
	// every entry matches and the CLBD agrees with the ledger.
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("ops-rt")
	mustPostOFX(t, l, "rt-1", acct, "equity", 25000, "EUR", base)
	mustPostOFX(t, l, "rt-2", "equity", acct, 4250, "EUR", base.Add(time.Hour))
	since := base.Add(-time.Hour)
	until := base.Add(24 * time.Hour)
	doc, err := l.ExportCAMT052(CAMT052ReportOptions{
		Account: acct, RptID: "RPT-RT", Since: since, Until: until,
	})
	if err != nil {
		t.Fatalf("ExportCAMT052: %v", err)
	}
	rep, err := l.ImportCAMT052(acct, doc, CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT052 of own export: %v", err)
	}
	if rep.Matched != 2 || rep.Mismatched != 0 || rep.Unmatched != 0 {
		t.Fatalf("summary = %+v, want 2/0/0 matched/mismatched/unmatched", rep)
	}
	if len(rep.Balances) != 2 {
		t.Fatalf("balances = %d, want OPBD+CLBD", len(rep.Balances))
	}
	if b := rep.Balances[0]; b.Type != CAMT052BalanceOPBD || b.AmountCents != 0 {
		t.Errorf("OPBD = %+v, want 0 (no rows before since)", b)
	}
	if b := rep.Balances[1]; b.Type != CAMT052BalanceCLBD || b.AmountCents != 20750 {
		t.Errorf("CLBD = %+v, want 20750 (25000 - 4250)", b)
	}
	if len(rep.BalanceMismatches) != 0 {
		t.Errorf("balance mismatches = %+v, want none on a round-trip", rep.BalanceMismatches)
	}
}

func TestCAMT052ImportRegistrySnapshotRoundTrip(t *testing.T) {
	src := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	mustPostOFX(t, src, "s-1", acct, "equity", 25000, "EUR", base)
	doc := camt052Doc("RPT-S", "IBAN:DE89370400440532013000",
		[]string{
			camt052Bal("OPBD", "0.00", "EUR", "CRDT", "2026-10-05"),
			camt052BalDtTm("CLBD", "250.00", "EUR", "CRDT", "2026-10-05T14:00:00Z"),
		},
		"        <Amt Ccy=\"EUR\">250.00</Amt>\n        <CdtDbtInd>CRDT</CdtDbtInd>\n        <BookgDt><Dt>"+base.Format("2006-01-02")+"</Dt></BookgDt>\n")
	if _, err := src.ImportCAMT052(acct, []byte(doc), CAMT053ImportOptions{}); err != nil {
		t.Fatalf("ImportCAMT052: %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !reflect.DeepEqual(src.camt052Imports, restored.camt052Imports) {
		t.Fatal("camt052 import registry did not survive the snapshot round-trip")
	}
	// The restored registry still dedupes: re-importing the same
	// document is a duplicate on the restored ledger too.
	rep, err := restored.ImportCAMT052(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("re-import on restored ledger: %v", err)
	}
	if !rep.Duplicate || rep.Matched != 1 {
		t.Fatalf("restored duplicate import = %+v, want duplicate with 1 match", rep)
	}
}
