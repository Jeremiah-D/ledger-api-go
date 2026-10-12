package ledger

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ofxSGMLDoc renders a minimal SGML-style OFX statement (unclosed
// tags, OFX 1.x shape) for account/currency with the given STMTTRN
// bodies. ofxXMLDoc renders the same statement in XML closed-tag
// style (OFX 2.x shape).
func ofxSGMLDoc(account, currency string, trns ...string) string {
	var sb strings.Builder
	sb.WriteString("OFXHEADER:100\nDATA:OFXSGML\nVERSION:102\nSECURITY:NONE\nENCODING:USASCII\nCHARSET:1252\nCOMPRESSION:NONE\nOLDFILEUID:NONE\nNEWFILEUID:NONE\n\n")
	sb.WriteString("<OFX>\n<SIGNONMSGSRSV1>\n<SONRS>\n<STATUS>\n<CODE>0\n<SEVERITY>INFO\n</STATUS>\n<DTSERVER>20261001120000\n<LANGUAGE>ENG\n</SONRS>\n</SIGNONMSGSRSV1>\n")
	sb.WriteString("<BANKMSGSRSV1>\n<STMTTRNRS>\n<TRNUID>1\n<STATUS>\n<CODE>0\n<SEVERITY>INFO\n</STATUS>\n<STMTRS>\n")
	sb.WriteString("<CURDEF>" + currency + "\n<BANKACCTFROM>\n<BANKID>LEDGER\n<ACCTID>" + account + "\n<ACCTTYPE>CHECKING\n</BANKACCTFROM>\n<BANKTRANLIST>\n<DTSTART>20261001000000\n<DTEND>20261002235959\n")
	for _, trn := range trns {
		sb.WriteString("<STMTTRN>\n" + trn + "\n")
	}
	sb.WriteString("</BANKTRANLIST>\n<LEDGERBAL>\n<BALAMT>110.00\n<DTASOF>20261002235959\n</LEDGERBAL>\n</STMTRS>\n</STMTTRNRS>\n</BANKMSGSRSV1>\n</OFX>\n")
	return sb.String()
}

func ofxXMLDoc(account, currency string, trns ...string) string {
	var sb strings.Builder
	sb.WriteString("OFXHEADER:100\nDATA:OFXSGML\nVERSION:211\nSECURITY:NONE\nENCODING:UTF-8\nCHARSET:UTF-8\nCOMPRESSION:NONE\nOLDFILEUID:NONE\nNEWFILEUID:NONE\n\n")
	sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<?OFX OFXHEADER=\"200\" VERSION=\"211\" SECURITY=\"NONE\" OLDFILEUID=\"NONE\" NEWFILEUID=\"NONE\"?>\n\n")
	sb.WriteString("<OFX>\n<SIGNONMSGSRSV1>\n<SONRS>\n<STATUS><CODE>0</CODE><SEVERITY>INFO</SEVERITY></STATUS>\n<DTSERVER>20261001120000</DTSERVER>\n<LANGUAGE>ENG</LANGUAGE>\n</SONRS>\n</SIGNONMSGSRSV1>\n")
	sb.WriteString("<BANKMSGSRSV1>\n<STMTTRNRS>\n<TRNUID>1</TRNUID>\n<STATUS><CODE>0</CODE><SEVERITY>INFO</SEVERITY></STATUS>\n<STMTRS>\n")
	sb.WriteString("<CURDEF>" + currency + "</CURDEF>\n<BANKACCTFROM>\n<BANKID>LEDGER</BANKID>\n<ACCTID>" + account + "</ACCTID>\n<ACCTTYPE>CHECKING</ACCTTYPE>\n</BANKACCTFROM>\n<BANKTRANLIST>\n<DTSTART>20261001000000</DTSTART>\n<DTEND>20261002235959</DTEND>\n")
	for _, trn := range trns {
		sb.WriteString("<STMTTRN>\n" + trn + "\n</STMTTRN>\n")
	}
	sb.WriteString("</BANKTRANLIST>\n<LEDGERBAL>\n<BALAMT>110.00</BALAMT>\n<DTASOF>20261002235959</DTASOF>\n</LEDGERBAL>\n</STMTRS>\n</STMTTRNRS>\n</BANKMSGSRSV1>\n</OFX>\n")
	return sb.String()
}

const ofxTrnCreditSGML = `<TRNTYPE>CREDIT
<DTPOSTED>20261001120000
<TRNAMT>150.00
<FITID>stmt-1
<MEMO>payroll &amp; bonus`

const ofxTrnDebitSGML = `<TRNTYPE>DEBIT
<DTPOSTED>20261002
<TRNAMT>-40.00
<FITID>stmt-2
<REFNUM>chk-1001`

const ofxTrnCreditXML = `<TRNTYPE>CREDIT</TRNTYPE>
<DTPOSTED>20261001120000</DTPOSTED>
<TRNAMT>150.00</TRNAMT>
<FITID>stmt-1</FITID>
<MEMO>payroll &amp; bonus</MEMO>`

const ofxTrnDebitXML = `<TRNTYPE>DEBIT</TRNTYPE>
<DTPOSTED>20261002</DTPOSTED>
<TRNAMT>-40.00</TRNAMT>
<FITID>stmt-2</FITID>
<REFNUM>chk-1001</REFNUM>`

func checkOFXPair(t *testing.T, txns []OFXTransaction) {
	t.Helper()
	if len(txns) != 2 {
		t.Fatalf("parsed %d transactions, want 2", len(txns))
	}
	cr := txns[0]
	if cr.Account != "cash" || cr.Currency != "USD" || cr.FITID != "stmt-1" {
		t.Errorf("credit txn identity = %+v, want cash/USD/stmt-1", cr)
	}
	if cr.Direction != OFXDirectionIn || cr.AmountCents != 15000 {
		t.Errorf("credit txn = dir %q amount %d, want in/15000", cr.Direction, cr.AmountCents)
	}
	if want := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC); !cr.PostedAt.Equal(want) {
		t.Errorf("credit PostedAt = %v, want %v", cr.PostedAt, want)
	}
	if cr.Memo != "payroll & bonus" {
		t.Errorf("credit Memo = %q, want entity-unescape", cr.Memo)
	}
	db := txns[1]
	if db.Direction != OFXDirectionOut || db.AmountCents != 4000 {
		t.Errorf("debit txn = dir %q amount %d, want out/4000", db.Direction, db.AmountCents)
	}
	if want := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC); !db.PostedAt.Equal(want) {
		t.Errorf("debit PostedAt = %v, want %v (date-only form)", db.PostedAt, want)
	}
	if db.Ref != "chk-1001" {
		t.Errorf("debit Ref = %q, want chk-1001", db.Ref)
	}
}

func TestParseOFXSGMLStyle(t *testing.T) {
	txns, err := ParseOFX([]byte(ofxSGMLDoc("cash", "USD", ofxTrnCreditSGML, ofxTrnDebitSGML)))
	if err != nil {
		t.Fatalf("ParseOFX SGML: %v", err)
	}
	checkOFXPair(t, txns)
}

func TestParseOFXXMLStyle(t *testing.T) {
	txns, err := ParseOFX([]byte(ofxXMLDoc("cash", "USD", ofxTrnCreditXML, ofxTrnDebitXML)))
	if err != nil {
		t.Fatalf("ParseOFX XML: %v", err)
	}
	checkOFXPair(t, txns)
}

// The import is the exact inverse of the export: ExportOFX output
// parses back to the same FITIDs, directions, and amounts.
func TestOFXExportParseRoundTrip(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "rt-1", "cash", "equity", 15000, "USD", base)
	mustPostOFX(t, l, "rt-2", "rent", "cash", 20000, "USD", base.Add(time.Hour))
	mustPostOFX(t, l, "rt-3", "cash", "equity", 999, "EUR", base.Add(2*time.Hour))

	for _, currency := range []string{"USD", "EUR"} {
		doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: currency})
		if err != nil {
			t.Fatalf("ExportOFX(%s): %v", currency, err)
		}
		txns, err := ParseOFX(doc)
		if err != nil {
			t.Fatalf("ParseOFX(export %s): %v", currency, err)
		}
		for _, txn := range txns {
			if txn.Account != "cash" || txn.Currency != currency {
				t.Errorf("txn %+v: want account cash currency %s", txn, currency)
			}
		}
	}
	doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	txns, err := ParseOFX(doc)
	if err != nil {
		t.Fatalf("ParseOFX: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("parsed %d txns, want 2", len(txns))
	}
	// rt-1: cash is the debit leg -> CREDIT/in, +150.00.
	if txns[0].FITID != "rt-1" || txns[0].Direction != OFXDirectionIn || txns[0].AmountCents != 15000 {
		t.Errorf("rt-1 parsed as %+v", txns[0])
	}
	// rt-2: cash is the credit leg -> DEBIT/out, 200.00.
	if txns[1].FITID != "rt-2" || txns[1].Direction != OFXDirectionOut || txns[1].AmountCents != 20000 {
		t.Errorf("rt-2 parsed as %+v", txns[1])
	}
	if !txns[0].PostedAt.Equal(base) || !txns[1].PostedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("PostedAt not preserved: %v %v", txns[0].PostedAt, txns[1].PostedAt)
	}
}

func TestParseOFXMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"whitespace":   "   \n  ",
		"garbage":      "this is not an OFX document at all",
		"no segments":  "<OFX>\n<SIGNONMSGSRSV1></SIGNONMSGSRSV1>\n</OFX>",
		"no curdef":    strings.Replace(ofxSGMLDoc("cash", "USD", ofxTrnCreditSGML), "<CURDEF>USD\n", "", 1),
		"bad currency": ofxSGMLDoc("cash", "XX", ofxTrnCreditSGML),
		"missing fitid": ofxSGMLDoc("cash", "USD",
			"<TRNTYPE>CREDIT\n<DTPOSTED>20261001120000\n<TRNAMT>150.00"),
		"bad amount": ofxSGMLDoc("cash", "USD",
			"<TRNTYPE>CREDIT\n<DTPOSTED>20261001120000\n<TRNAMT>fifteen\n<FITID>stmt-9"),
		"missing amount": ofxSGMLDoc("cash", "USD",
			"<TRNTYPE>CREDIT\n<DTPOSTED>20261001120000\n<FITID>stmt-9"),
		"bad date": ofxSGMLDoc("cash", "USD",
			"<TRNTYPE>CREDIT\n<DTPOSTED>not-a-date\n<TRNAMT>150.00\n<FITID>stmt-9"),
		"missing date": ofxSGMLDoc("cash", "USD",
			"<TRNTYPE>CREDIT\n<TRNAMT>150.00\n<FITID>stmt-9"),
		"trn outside statement": "<OFX>\n<STMTTRN>\n<TRNTYPE>CREDIT\n<DTPOSTED>20261001120000\n<TRNAMT>1.00\n<FITID>x\n</OFX>",
	}
	for name, doc := range cases {
		if _, err := ParseOFX([]byte(doc)); !errors.Is(err, ErrInvalidOFX) {
			t.Errorf("%s: err = %v, want ErrInvalidOFX", name, err)
		}
	}
}

func TestParseOFXAmountPrecision(t *testing.T) {
	cases := map[string]int64{
		"150.00": 15000, "-40.00": -4000, "100": 10000, "+25.5": 2550,
		"0.01": 1, "0.005": 1, "0.004": 0, "-0.005": -1, ".99": 99,
		"1234567.89": 123456789,
	}
	for raw, want := range cases {
		got, err := parseOFXAmount(raw)
		if err != nil {
			t.Errorf("parseOFXAmount(%q): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("parseOFXAmount(%q) = %d, want %d", raw, got, want)
		}
	}
	for _, raw := range []string{"", "abc", "1.2.3", "--5", "1,000.00", "NaN"} {
		if _, err := parseOFXAmount(raw); !errors.Is(err, ErrInvalidOFX) {
			t.Errorf("parseOFXAmount(%q): err = %v, want ErrInvalidOFX", raw, err)
		}
	}
}

func TestImportOFXMatchStates(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// One unique entry -> matched.
	mustPostOFX(t, l, "m-1", "cash", "equity", 15000, "USD", base)
	// Two entries with the same |amount| inside the window -> mismatched.
	mustPostOFX(t, l, "m-2a", "cash", "equity", 7777, "USD", base.Add(-time.Hour))
	mustPostOFX(t, l, "m-2b", "cash", "equity", 7777, "USD", base.Add(time.Hour))
	// An entry outside the default 24h window must not match.
	mustPostOFX(t, l, "m-far", "cash", "equity", 4242, "USD", base.Add(-72*time.Hour))

	doc := ofxSGMLDoc("cash", "USD",
		"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>150.00\n<FITID>imp-1",
		"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>77.77\n<FITID>imp-2",
		"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>42.42\n<FITID>imp-3",
		"<TRNTYPE>DEBIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>-9.99\n<FITID>imp-4",
	)
	_, linksBefore := l.ChainHead()
	rep, err := l.ImportOFX("cash", []byte(doc), OFXImportOptions{})
	if err != nil {
		t.Fatalf("ImportOFX: %v", err)
	}
	if rep.Matched != 1 || rep.Mismatched != 1 || rep.Unmatched != 2 {
		t.Fatalf("summary = %+v, want 1/1/2 matched/mismatched/unmatched", rep)
	}
	if rep.Duplicate {
		t.Error("first import must not be a duplicate")
	}
	byFITID := map[string]OFXImportItem{}
	for _, it := range rep.Items {
		byFITID[it.Transaction.FITID] = it
	}
	if it := byFITID["imp-1"]; it.Status != OFXMatched || it.MatchedEntryID != "m-1" {
		t.Errorf("imp-1 = %+v, want matched to m-1", it)
	}
	if it := byFITID["imp-2"]; it.Status != OFXMismatched || it.CandidateCount != 2 {
		t.Errorf("imp-2 = %+v, want mismatched with 2 candidates", it)
	}
	if it := byFITID["imp-3"]; it.Status != OFXUnmatched {
		t.Errorf("imp-3 = %+v, want unmatched (only candidate outside window)", it)
	}
	if it := byFITID["imp-4"]; it.Status != OFXUnmatched {
		t.Errorf("imp-4 = %+v, want unmatched", it)
	}
	// Draft shape: "ofx/<FITID>" ID, the OFX account on its leg, the
	// counterparty empty (the operator supplies it when booking).
	d := byFITID["imp-1"].Draft
	if d.ID != "ofx/imp-1" || d.DebitAccount != "cash" || d.CreditAccount != "" || d.AmountCents != 15000 || d.Currency != "USD" {
		t.Errorf("imp-1 draft = %+v", d)
	}
	d4 := byFITID["imp-4"].Draft
	if d4.ID != "ofx/imp-4" || d4.CreditAccount != "cash" || d4.DebitAccount != "" || d4.AmountCents != 999 {
		t.Errorf("imp-4 draft = %+v", d4)
	}
	// The import is a draft: no journal rows, no chain links, no
	// version movement.
	if _, linksAfter := l.ChainHead(); linksAfter != linksBefore {
		t.Errorf("chain links moved %d -> %d on import", linksBefore, linksAfter)
	}
}

func TestImportOFXCustomWindow(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "w-1", "cash", "equity", 5000, "USD", base.Add(-48*time.Hour))
	doc := ofxSGMLDoc("cash", "USD",
		"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>50.00\n<FITID>wimp-1")
	rep, err := l.ImportOFX("cash", []byte(doc), OFXImportOptions{})
	if err != nil {
		t.Fatalf("ImportOFX: %v", err)
	}
	if rep.Unmatched != 1 {
		t.Fatalf("default window: %+v, want unmatched", rep)
	}
	rep, err = l.ImportOFX("cash", []byte(doc+" "), OFXImportOptions{MatchWindow: 72 * time.Hour})
	if err != nil {
		t.Fatalf("ImportOFX wide window: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedEntryID != "w-1" {
		t.Fatalf("72h window: %+v, want matched to w-1", rep)
	}
}

func TestImportOFXDuplicate(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "d-1", "cash", "equity", 15000, "USD", base)
	doc := ofxSGMLDoc("cash", "USD",
		"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>150.00\n<FITID>dup-1")
	first, err := l.ImportOFX("cash", []byte(doc), OFXImportOptions{})
	if err != nil {
		t.Fatalf("first ImportOFX: %v", err)
	}
	second, err := l.ImportOFX("cash", []byte(doc), OFXImportOptions{})
	if err != nil {
		t.Fatalf("second ImportOFX: %v", err)
	}
	if !second.Duplicate {
		t.Error("second import of the same document must report duplicate=true")
	}
	first.Duplicate, second.Duplicate = false, false
	if !reflect.DeepEqual(first, second) {
		t.Errorf("duplicate report differs from the original:\n%+v\n%+v", first, second)
	}
	// A different document is a new import, not a duplicate.
	third, err := l.ImportOFX("cash", []byte(doc+" "), OFXImportOptions{})
	if err != nil {
		t.Fatalf("third ImportOFX: %v", err)
	}
	if third.Duplicate {
		t.Error("a different document must not be a duplicate")
	}
}

func TestImportOFXAccountMismatch(t *testing.T) {
	l := New()
	doc := ofxSGMLDoc("savings", "USD", ofxTrnCreditSGML)
	if _, err := l.ImportOFX("cash", []byte(doc), OFXImportOptions{}); !errors.Is(err, ErrInvalidOFX) {
		t.Fatalf("account mismatch: err = %v, want ErrInvalidOFX", err)
	}
	if _, err := l.ImportOFX("", []byte(doc), OFXImportOptions{}); err != ErrEmptyAccountID {
		t.Fatalf("empty account: err = %v, want ErrEmptyAccountID", err)
	}
}

func TestImportOFXExcludesPendingReview(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// A pending-review row never touched balances: like the exporter,
	// the importer must not treat it as booked. The row below is a
	// large transfer held by the dual-control review (same setup as
	// TestOFXExcludesPendingReview).
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 50000, Currency: "USD", CreatedAt: base}); err != nil {
		t.Fatalf("Post seed: %v", err)
	}
	if err := l.SetReviewThreshold("cash", 1000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "t-pr", From: "cash", To: "rent", AmountCents: 15000, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	doc := ofxSGMLDoc("cash", "USD",
		"<TRNTYPE>DEBIT\n<DTPOSTED>"+time.Now().UTC().Format("20060102150405")+"\n<TRNAMT>-150.00\n<FITID>pr-imp")
	rep, err := l.ImportOFX("cash", []byte(doc), OFXImportOptions{})
	if err != nil {
		t.Fatalf("ImportOFX: %v", err)
	}
	if rep.Unmatched != 1 || rep.Items[0].Status != OFXUnmatched {
		t.Fatalf("pending-review row must not match: %+v", rep.Items[0])
	}
}

func TestOFXImportRegistrySnapshotRoundTrip(t *testing.T) {
	src := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, src, "s-1", "cash", "equity", 15000, "USD", base)
	doc := ofxSGMLDoc("cash", "USD",
		"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>150.00\n<FITID>snap-1")
	if _, err := src.ImportOFX("cash", []byte(doc), OFXImportOptions{}); err != nil {
		t.Fatalf("ImportOFX: %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !reflect.DeepEqual(src.ofxImports, restored.ofxImports) {
		t.Fatal("ofx import registry did not survive the snapshot round-trip")
	}
	// The restored registry still dedupes: re-importing the same
	// document is a duplicate on the restored ledger too.
	rep, err := restored.ImportOFX("cash", []byte(doc), OFXImportOptions{})
	if err != nil {
		t.Fatalf("re-import on restored ledger: %v", err)
	}
	if !rep.Duplicate || rep.Matched != 1 {
		t.Fatalf("restored duplicate import = %+v, want duplicate with 1 match", rep)
	}
}
