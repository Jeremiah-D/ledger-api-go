package ledger

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// camt053Doc renders a realistic camt.053.001.08 statement document for
// the given IBAN (empty means the Other/Id shape instead) with the
// given Ntry bodies. It carries the official default namespace so the
// test proves namespace tolerance, and includes optional blocks (Bal,
// Chrgs) that a strict positional parser would choke on.
func camt053Doc(stmtID, acct string, entries ...string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08">` + "\n")
	sb.WriteString("  <BkToCstmrStmt>\n    <GrpHdr>\n      <MsgId>MSG-20261016-001</MsgId>\n      <CreDtTm>2026-10-16T08:00:00Z</CreDtTm>\n    </GrpHdr>\n    <Stmt>\n")
	sb.WriteString("      <Id>" + stmtID + "</Id>\n")
	sb.WriteString("      <ElctrncSeqNb>7</ElctrncSeqNb>\n")
	if strings.HasPrefix(acct, "IBAN:") {
		sb.WriteString("      <Acct>\n        <Id>\n          <IBAN>" + strings.TrimPrefix(acct, "IBAN:") + "</IBAN>\n        </Id>\n        <Ccy>EUR</Ccy>\n      </Acct>\n")
	} else {
		sb.WriteString("      <Acct>\n        <Id>\n          <Othr>\n            <Id>" + acct + "</Id>\n          </Othr>\n        </Id>\n        <Ccy>USD</Ccy>\n      </Acct>\n")
	}
	sb.WriteString("      <Bal>\n        <Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp>\n        <Amt Ccy=\"EUR\">1000.00</Amt>\n        <CdtDbtInd>CRDT</CdtDbtInd>\n        <Dt><Dt>2026-10-15</Dt></Dt>\n      </Bal>\n")
	for _, e := range entries {
		sb.WriteString("      <Ntry>\n" + e + "      </Ntry>\n")
	}
	sb.WriteString("    </Stmt>\n  </BkToCstmrStmt>\n</Document>\n")
	return sb.String()
}

const camt053NtryIn = `        <Amt Ccy="EUR">250.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Sts>BOOK</Sts>
        <BookgDt><Dt>2026-10-15</Dt></BookgDt>
        <ValDt><Dt>2026-10-15</Dt></ValDt>
        <NtryDtls>
          <TxDtls>
            <Refs><EndToEndId>E2E-INV-1042</EndToEndId></Refs>
            <RmtInf><Ustrd>Invoice 1042</Ustrd><Ustrd>October services</Ustrd></RmtInf>
          </TxDtls>
        </NtryDtls>
`

const camt053NtryOut = `        <Amt Ccy="EUR">42.50</Amt>
        <CdtDbtInd>DBIT</CdtDbtInd>
        <Sts>BOOK</Sts>
        <BookgDt><DtTm>2026-10-15T14:30:00+02:00</DtTm></BookgDt>
        <NtryDtls>
          <TxDtls>
            <Refs><EndToEndId>E2E-RENT-09</EndToEndId></Refs>
            <RmtInf><Ustrd>Office rent</Ustrd></RmtInf>
          </TxDtls>
        </NtryDtls>
`

func TestParseCAMT053SampleVector(t *testing.T) {
	doc := camt053Doc("STMT-2026-10", "IBAN:DE89370400440532013000", camt053NtryIn, camt053NtryOut)
	txns, err := ParseCAMT053([]byte(doc))
	if err != nil {
		t.Fatalf("ParseCAMT053: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(txns))
	}
	in := txns[0]
	if in.Account != "DE89370400440532013000" || in.Currency != "EUR" || in.StmtID != "STMT-2026-10" || in.Seq != 1 {
		t.Errorf("in entry identity = %+v", in)
	}
	if in.Direction != CAMT053DirectionIn || in.AmountCents != 25000 {
		t.Errorf("in entry = dir %q amount %d, want in/25000", in.Direction, in.AmountCents)
	}
	if want := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC); !in.PostedAt.Equal(want) {
		t.Errorf("in PostedAt = %v, want %v", in.PostedAt, want)
	}
	if in.Memo != "Invoice 1042 | October services" || in.EndToEndID != "E2E-INV-1042" {
		t.Errorf("in memo/e2e = %q/%q", in.Memo, in.EndToEndID)
	}
	out := txns[1]
	if out.Direction != CAMT053DirectionOut || out.AmountCents != 4250 {
		t.Errorf("out entry = dir %q amount %d, want out/4250", out.Direction, out.AmountCents)
	}
	// The +02:00 datetime normalizes to UTC.
	if want := time.Date(2026, 10, 15, 12, 30, 0, 0, time.UTC); !out.PostedAt.Equal(want) {
		t.Errorf("out PostedAt = %v, want %v", out.PostedAt, want)
	}
	if out.Seq != 2 {
		t.Errorf("out Seq = %d, want 2", out.Seq)
	}
	// Draft shape: "camt053/<stmtID>/<seq>", the statement account on
	// its leg, the counterparty empty (the operator books it).
	d := in.Draft()
	if d.ID != "camt053/STMT-2026-10/1" || d.DebitAccount != "DE89370400440532013000" ||
		d.CreditAccount != "" || d.AmountCents != 25000 || d.Currency != "EUR" ||
		d.ExternalRef != "E2E-INV-1042" {
		t.Errorf("in draft = %+v", d)
	}
	d2 := out.Draft()
	if d2.ID != "camt053/STMT-2026-10/2" || d2.CreditAccount != "DE89370400440532013000" ||
		d2.DebitAccount != "" || d2.AmountCents != 4250 {
		t.Errorf("out draft = %+v", d2)
	}
}

func TestParseCAMT053OtherAccountID(t *testing.T) {
	doc := camt053Doc("STMT-9", "cash", camt053NtryIn)
	txns, err := ParseCAMT053([]byte(doc))
	if err != nil {
		t.Fatalf("ParseCAMT053: %v", err)
	}
	if len(txns) != 1 || txns[0].Account != "cash" {
		t.Fatalf("account = %+v, want cash", txns)
	}
}

func TestParseCAMT053Rejects(t *testing.T) {
	good := camt053Doc("STMT-1", "IBAN:DE89370400440532013000", camt053NtryIn)
	cases := []struct {
		name string
		mut  func(string) string
	}{
		{"empty", func(string) string { return "  \n" }},
		{"not XML", func(string) string { return "<Document><broken" }},
		{"no Stmt", func(s string) string { return strings.Replace(s, "<Stmt>", "<Nope>", 1) }},
		{"Stmt without Id", func(s string) string { return strings.Replace(s, "<Id>STMT-1</Id>", "<Id></Id>", 1) }},
		{"missing Amt", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">250.00</Amt>`, "", 1)
		}},
		{"bad Amt", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">250.00</Amt>`, `<Amt Ccy="EUR">abc</Amt>`, 1)
		}},
		{"bad Ccy", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">250.00</Amt>`, `<Amt Ccy="EURO">250.00</Amt>`, 1)
		}},
		{"missing Ccy", func(s string) string {
			return strings.Replace(s, `<Amt Ccy="EUR">250.00</Amt>`, `<Amt>250.00</Amt>`, 1)
		}},
		{"missing booking date", func(s string) string {
			s = strings.Replace(s, "<BookgDt><Dt>2026-10-15</Dt></BookgDt>", "", 1)
			return strings.Replace(s, "<ValDt><Dt>2026-10-15</Dt></ValDt>", "", 1)
		}},
		{"bad date", func(s string) string {
			return strings.Replace(s, "<BookgDt><Dt>2026-10-15</Dt></BookgDt>",
				"<BookgDt><Dt>15.10.2026</Dt></BookgDt>", 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCAMT053([]byte(tc.mut(good))); !errors.Is(err, ErrInvalidCAMT053) {
				t.Fatalf("err = %v, want ErrInvalidCAMT053", err)
			}
		})
	}
}

func TestImportCAMT053MatchStates(t *testing.T) {
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
	doc := camt053Doc("STMT-M", "IBAN:DE89370400440532013000",
		ntry("250.00", "EUR", "CRDT", day),
		ntry("77.77", "EUR", "CRDT", day),
		ntry("42.42", "EUR", "CRDT", day),
		ntry("9.99", "EUR", "DBIT", day),
	)
	_, linksBefore := l.ChainHead()
	rep, err := l.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT053: %v", err)
	}
	if rep.Matched != 1 || rep.Mismatched != 1 || rep.Unmatched != 2 {
		t.Fatalf("summary = %+v, want 1/1/2 matched/mismatched/unmatched", rep)
	}
	if rep.Duplicate {
		t.Error("first import must not be a duplicate")
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
	// Draft shape: "camt053/<stmtID>/<seq>", the statement account on
	// its leg, the counterparty empty.
	d := bySeq[1].Draft
	if d.ID != "camt053/STMT-M/1" || d.DebitAccount != acct || d.CreditAccount != "" ||
		d.AmountCents != 25000 || d.Currency != "EUR" {
		t.Errorf("seq 1 draft = %+v", d)
	}
	d4 := bySeq[4].Draft
	if d4.ID != "camt053/STMT-M/4" || d4.CreditAccount != acct || d4.DebitAccount != "" || d4.AmountCents != 999 {
		t.Errorf("seq 4 draft = %+v", d4)
	}
	// The import is a draft: no journal rows, no chain links, no
	// version movement.
	if _, linksAfter := l.ChainHead(); linksAfter != linksBefore {
		t.Errorf("chain links moved %d -> %d on import", linksBefore, linksAfter)
	}
}

func TestImportCAMT053CustomWindow(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	mustPostOFX(t, l, "w-1", acct, "equity", 4242, "EUR", base.Add(-72*time.Hour))
	doc := camt053Doc("STMT-W", "IBAN:DE89370400440532013000",
		"        <Amt Ccy=\"EUR\">42.42</Amt>\n        <CdtDbtInd>CRDT</CdtDbtInd>\n        <BookgDt><Dt>"+base.Format("2006-01-02")+"</Dt></BookgDt>\n")
	rep, err := l.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{MatchWindow: 96 * time.Hour})
	if err != nil {
		t.Fatalf("ImportCAMT053: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedEntryID != "w-1" {
		t.Fatalf("custom window should match: %+v", rep.Items[0])
	}
}

func TestImportCAMT053Duplicate(t *testing.T) {
	l := New()
	acct := AccountID("DE89370400440532013000")
	doc := camt053Doc("STMT-D", "IBAN:DE89370400440532013000", camt053NtryIn)
	first, err := l.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("first ImportCAMT053: %v", err)
	}
	if first.DocumentSHA256 == "" {
		t.Fatal("report has no document SHA-256")
	}
	second, err := l.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("second ImportCAMT053: %v", err)
	}
	if !second.Duplicate {
		t.Error("re-import must be marked duplicate")
	}
	if !reflect.DeepEqual(first.Items, second.Items) || first.Matched != second.Matched {
		t.Error("duplicate report must replay the original report")
	}
}

func TestImportCAMT053AccountMismatch(t *testing.T) {
	l := New()
	doc := camt053Doc("STMT-X", "IBAN:DE89370400440532013000", camt053NtryIn)
	if _, err := l.ImportCAMT053("cash", []byte(doc), CAMT053ImportOptions{}); !errors.Is(err, ErrInvalidCAMT053) {
		t.Fatalf("err = %v, want ErrInvalidCAMT053", err)
	}
}

func TestImportCAMT053ExcludesPendingReview(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: acct, CreditAccount: "equity", AmountCents: 50000, Currency: "EUR", CreatedAt: base}); err != nil {
		t.Fatalf("Post seed: %v", err)
	}
	if err := l.SetReviewThreshold(acct, 1000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "t-pr", From: acct, To: "rent", AmountCents: 15000, Currency: "EUR"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	doc := camt053Doc("STMT-PR", "IBAN:DE89370400440532013000",
		"        <Amt Ccy=\"EUR\">150.00</Amt>\n        <CdtDbtInd>DBIT</CdtDbtInd>\n        <BookgDt><Dt>"+time.Now().UTC().Format("2006-01-02")+"</Dt></BookgDt>\n")
	rep, err := l.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT053: %v", err)
	}
	if rep.Unmatched != 1 || rep.Items[0].Status != CAMT053Unmatched {
		t.Fatalf("pending-review row must not match: %+v", rep.Items[0])
	}
}

func TestCAMT053ImportRegistrySnapshotRoundTrip(t *testing.T) {
	src := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	mustPostOFX(t, src, "s-1", acct, "equity", 25000, "EUR", base)
	doc := camt053Doc("STMT-S", "IBAN:DE89370400440532013000",
		"        <Amt Ccy=\"EUR\">250.00</Amt>\n        <CdtDbtInd>CRDT</CdtDbtInd>\n        <BookgDt><Dt>"+base.Format("2006-01-02")+"</Dt></BookgDt>\n")
	if _, err := src.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{}); err != nil {
		t.Fatalf("ImportCAMT053: %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !reflect.DeepEqual(src.camt053Imports, restored.camt053Imports) {
		t.Fatal("camt053 import registry did not survive the snapshot round-trip")
	}
	// The restored registry still dedupes: re-importing the same
	// document is a duplicate on the restored ledger too.
	rep, err := restored.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("re-import on restored ledger: %v", err)
	}
	if !rep.Duplicate || rep.Matched != 1 {
		t.Fatalf("restored duplicate import = %+v, want duplicate with 1 match", rep)
	}
}

func TestParseCAMT053MultiStatement(t *testing.T) {
	// Two Stmt blocks in one document: entries sequence per statement,
	// and draft IDs stay unique across statements.
	doc := camt053Doc("STMT-A", "IBAN:DE89370400440532013000", camt053NtryIn)
	doc2 := strings.Replace(doc, "</Stmt>", "</Stmt>\n    <Stmt>\n      <Id>STMT-B</Id>\n      <Acct>\n        <Id>\n          <IBAN>DE89370400440532013000</IBAN>\n        </Id>\n      </Acct>\n      <Ntry>\n"+camt053NtryOut+"      </Ntry>\n    </Stmt>", 1)
	txns, err := ParseCAMT053([]byte(doc2))
	if err != nil {
		t.Fatalf("ParseCAMT053: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(txns))
	}
	if txns[0].StmtID != "STMT-A" || txns[0].Seq != 1 || txns[1].StmtID != "STMT-B" || txns[1].Seq != 1 {
		t.Fatalf("stmt/seq = %+v", txns)
	}
	if d1, d2 := txns[0].Draft().ID, txns[1].Draft().ID; d1 == d2 {
		t.Fatalf("draft IDs collide: %q", d1)
	}
}
