package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pacs003InboundLedger creates a ledger with a funded payer and one
// active mandate whose validity window opens 48h ago: the inbound
// collection date is a UTC calendar date (midnight), so the window
// must already cover the start of today, unlike mandateFixture's
// now-minus-one-hour ValidFrom.
func pacs003InboundLedger(t *testing.T, opts ...Option) (*Ledger, Mandate) {
	t.Helper()
	l := New(opts...)
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	m, err := l.CreateMandate(Mandate{
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 500000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-48 * time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return l, m
}

func pacs003InboundFixture(t *testing.T) (*Ledger, Mandate) {
	t.Helper()
	return pacs003InboundLedger(t)
}

// pacs003Doc renders one inbound pacs.003.001.08 instruction: msgID
// in GrpHdr (with IntrBkSttlmDt as the requested collection date
// when settleDate is non-empty), one DrctDbtTxInf pulling amount in
// ccy under mandateID, EndToEndId e2e, signature date dtOfSgntr.
func pacs003Doc(msgID, e2e, amount, ccy, mandateID, settleDate, dtOfSgntr string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.003.001.08">`)
	sb.WriteString(`<FIToFICstmrDrctDbt><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T07:00:00Z</CreDtTm><NbOfTxs>1</NbOfTxs>`)
	if settleDate != "" {
		sb.WriteString(`<IntrBkSttlmDt>` + settleDate + `</IntrBkSttlmDt>`)
	}
	sb.WriteString(`<InitgPty><FinInstnId><Othr><Id>BANKCRED</Id></Othr></FinInstnId></InitgPty></GrpHdr>`)
	sb.WriteString(`<DrctDbtTxInf><PmtId><InstrId>instr-1</InstrId><EndToEndId>` + e2e + `</EndToEndId><TxId>tx-1</TxId></PmtId>`)
	sb.WriteString(`<InstdAmt Ccy="` + ccy + `">` + amount + `</InstdAmt>`)
	sb.WriteString(`<DrctDbtTx><MndtRltdInf><MndtId>` + mandateID + `</MndtId>`)
	if dtOfSgntr != "" {
		sb.WriteString(`<DtOfSgntr>` + dtOfSgntr + `</DtOfSgntr>`)
	}
	sb.WriteString(`</MndtRltdInf></DrctDbtTx>`)
	sb.WriteString(`<CdtrAgt><FinInstnId><Othr><Id>BANKCRED</Id></Othr></FinInstnId></CdtrAgt>`)
	sb.WriteString(`<Cdtr><Nm>Merchant</Nm><Id><PrvtId><Othr><Id>merchant</Id></Othr></PrvtId></Id></Cdtr>`)
	sb.WriteString(`<CdtrAcct><Id><Othr><Id>merchant</Id></Othr></Id></CdtrAcct>`)
	sb.WriteString(`<DbtrAgt><FinInstnId><Othr><Id>BANKDEBB</Id></Othr></FinInstnId></DbtrAgt>`)
	sb.WriteString(`<Dbtr><Nm>Customer</Nm><Id><PrvtId><Othr><Id>cust</Id></Othr></PrvtId></Id></Dbtr>`)
	sb.WriteString(`<DbtrAcct><Id><Othr><Id>cust</Id></Othr></Id></DbtrAcct>`)
	sb.WriteString(`</DrctDbtTxInf></FIToFICstmrDrctDbt></Document>`)
	return sb.String()
}

func TestParsePacs003(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	d, err := ParsePacs003([]byte(pacs003Doc("msg-1", "e2e-1", "2500.00", "USD", "m-sub", today, "2026-01-15")))
	if err != nil {
		t.Fatalf("ParsePacs003: %v", err)
	}
	if d.MsgID != "msg-1" || d.InstrID != "instr-1" || d.EndToEndID != "e2e-1" || d.TxID != "tx-1" {
		t.Errorf("identity = %+v, want msg-1/instr-1/e2e-1/tx-1", d)
	}
	if d.AmountCents != 250000 || d.Currency != "USD" {
		t.Errorf("amount = %d %s, want 250000 USD", d.AmountCents, d.Currency)
	}
	if d.MandateID != "m-sub" || d.MandateSignatureDate != "2026-01-15" {
		t.Errorf("mandate = %q signed %q, want m-sub 2026-01-15", d.MandateID, d.MandateSignatureDate)
	}
	if got := d.CollectionDate.UTC().Format("2006-01-02"); got != today {
		t.Errorf("collection date = %q, want %q", got, today)
	}
	if d.Debtor != "cust" || d.Creditor != "merchant" || d.DebtorAgent != "BANKDEBB" || d.CreditorAgent != "BANKCRED" {
		t.Errorf("parties = %q/%q agents = %q/%q, want cust/merchant BANKDEBB/BANKCRED", d.Debtor, d.Creditor, d.DebtorAgent, d.CreditorAgent)
	}

	// No settlement date stated: the debit books at now.
	d, err = ParsePacs003([]byte(pacs003Doc("msg-2", "e2e-2", "10.50", "USD", "m-sub", "", "")))
	if err != nil {
		t.Fatalf("ParsePacs003 without settle date: %v", err)
	}
	if !d.CollectionDate.IsZero() {
		t.Errorf("collection date = %v, want zero (books at now)", d.CollectionDate)
	}
	if d.AmountCents != 1050 {
		t.Errorf("amount = %d, want 1050", d.AmountCents)
	}
}

func TestParsePacs003Rejections(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	cases := []struct {
		name string
		doc  string
	}{
		{"malformed XML", `<Document><broken`},
		{"empty document", `  `},
		{"wrong namespace", strings.Replace(pacs003Doc("m", "e", "10.00", "USD", "m-sub", today, ""), "pacs.003.001.08", "pacs.003.001.09", 1)},
		{"missing MsgId", pacs003Doc("", "e2e-x", "10.00", "USD", "m-sub", today, "")},
		{"missing EndToEndId", pacs003Doc("msg-x", "", "10.00", "USD", "m-sub", today, "")},
		{"missing MndtId", pacs003Doc("msg-x", "e2e-x", "10.00", "USD", "", today, "")},
		{"zero amount", pacs003Doc("msg-x", "e2e-x", "0.00", "USD", "m-sub", today, "")},
		{"negative amount", pacs003Doc("msg-x", "e2e-x", "-5.00", "USD", "m-sub", today, "")},
		{"non-numeric amount", pacs003Doc("msg-x", "e2e-x", "10.005", "USD", "m-sub", today, "")},
		{"empty currency", pacs003Doc("msg-x", "e2e-x", "10.00", "", "m-sub", today, "")},
		{"lowercase currency", pacs003Doc("msg-x", "e2e-x", "10.00", "usd", "m-sub", today, "")},
		{"malformed signature date", pacs003Doc("msg-x", "e2e-x", "10.00", "USD", "m-sub", today, "15-01-2026")},
		{"malformed settlement date", pacs003Doc("msg-x", "e2e-x", "10.00", "USD", "m-sub", "14/11/2026", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePacs003([]byte(tc.doc)); !errors.Is(err, ErrPacs003Parse) {
				t.Errorf("ParsePacs003 err = %v, want ErrPacs003Parse", err)
			}
		})
	}
}

// applyPacs003Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyPacs003Doc(t *testing.T, l *Ledger, doc string) (MandateDebitReceipt, error) {
	t.Helper()
	d, err := ParsePacs003([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePacs003: %v", err)
	}
	return l.ApplyPacs003(d, "creditor-bank")
}

func TestApplyPacs003Success(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pacs003InboundLedger(t, WithAuditLog(al))
	before := l.Version()
	today := time.Now().UTC().Format("2006-01-02")
	receipt, err := applyPacs003Doc(t, l, pacs003Doc("msg-ok", "e2e-ok", "2500.00", "USD", "m-sub", today, "2026-01-15"))
	if err != nil {
		t.Fatalf("ApplyPacs003: %v", err)
	}
	if receipt.Duplicate {
		t.Errorf("duplicate = true, want false")
	}
	if receipt.MandateID != "m-sub" {
		t.Errorf("mandate_id = %q, want m-sub", receipt.MandateID)
	}
	e := receipt.Entry
	// The entry ID is the instruction's EndToEndId; the legs come
	// from the mandate (debit payee, credit payer), not the document.
	if e.ID != "e2e-ok" {
		t.Errorf("entry ID = %q, want e2e-ok", e.ID)
	}
	if e.DebitAccount != "merchant" || e.CreditAccount != "cust" {
		t.Errorf("legs = %s/%s, want merchant/cust", e.DebitAccount, e.CreditAccount)
	}
	if e.AmountCents != 250000 || e.Currency != "USD" {
		t.Errorf("entry = %+v, want 250000 USD", e)
	}
	if e.Memo != "pacs003:msg-ok mandate_of:m-sub" {
		t.Errorf("memo = %q, want %q", e.Memo, "pacs003:msg-ok mandate_of:m-sub")
	}
	// The collection date is the booking timestamp.
	if got := e.CreatedAt.UTC().Format("2006-01-02"); got != today {
		t.Errorf("entry CreatedAt = %q, want %q", got, today)
	}
	if l.Version() != before+1 {
		t.Errorf("version = %d, want %d", l.Version(), before+1)
	}
	if got := l.BalanceIn("cust", "USD"); got != 1000000-250000 {
		t.Errorf("cust balance = %d, want %d", got, 1000000-250000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 250000 {
		t.Errorf("merchant balance = %d, want %d", got, 250000)
	}
	if n := l.MandateDebitCount(); n != 1 {
		t.Errorf("MandateDebitCount = %d, want 1", n)
	}
	// Audit: exactly one inbound marker plus the mandate_debit event
	// DebitWithMandate emits.
	events := readAuditOps(t, al)
	if countOp(events, "pacs003_debit_applied") != 1 {
		t.Errorf("pacs003_debit_applied count = %d, want 1", countOp(events, "pacs003_debit_applied"))
	}
	if countOp(events, "mandate_debit") != 1 {
		t.Errorf("mandate_debit count = %d, want 1", countOp(events, "mandate_debit"))
	}
	ev := findOp(events, "pacs003_debit_applied")
	if ev == nil {
		t.Fatalf("pacs003_debit_applied event missing")
	}
	if ev.TraceID != "msg-ok" {
		t.Errorf("trace = %q, want msg-ok", ev.TraceID)
	}
	if len(ev.EntryIDs) != 1 || ev.EntryIDs[0] != "e2e-ok" {
		t.Errorf("entries = %v, want [e2e-ok]", ev.EntryIDs)
	}
}

func TestApplyPacs003Gates(t *testing.T) {
	now := time.Now()
	today := now.UTC().Format("2006-01-02")
	cases := []struct {
		name    string
		setup   func(t *testing.T, l *Ledger)
		doc     string
		wantErr error
	}{
		{
			"unknown mandate",
			func(t *testing.T, l *Ledger) {},
			pacs003Doc("msg-u", "e2e-u", "10.00", "USD", "m-no-such", today, ""),
			ErrMandateNotFound,
		},
		{
			"suspended mandate",
			func(t *testing.T, l *Ledger) {
				if _, err := l.SuspendMandate("m-sub"); err != nil {
					t.Fatalf("SuspendMandate: %v", err)
				}
			},
			pacs003Doc("msg-s", "e2e-s", "10.00", "USD", "m-sub", today, ""),
			ErrMandateNotActive,
		},
		{
			"collection after ValidUntil",
			func(t *testing.T, l *Ledger) {},
			pacs003Doc("msg-f", "e2e-f", "10.00", "USD", "m-sub", now.Add(31*24*time.Hour).UTC().Format("2006-01-02"), ""),
			ErrMandateExpired,
		},
		{
			"collection before ValidFrom",
			func(t *testing.T, l *Ledger) {},
			pacs003Doc("msg-p", "e2e-p", "10.00", "USD", "m-sub", now.Add(-72*time.Hour).UTC().Format("2006-01-02"), ""),
			ErrMandateNotActive,
		},
		{
			"amount over cap",
			func(t *testing.T, l *Ledger) {},
			pacs003Doc("msg-o", "e2e-o", "5000.01", "USD", "m-sub", today, ""),
			ErrMandateAmountExceeded,
		},
		{
			"currency mismatch",
			func(t *testing.T, l *Ledger) {},
			pacs003Doc("msg-c", "e2e-c", "10.00", "EUR", "m-sub", today, ""),
			ErrMandateCurrencyMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := pacs003InboundFixture(t)
			tc.setup(t, l)
			before := l.Version()
			_, err := applyPacs003Doc(t, l, tc.doc)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ApplyPacs003 err = %v, want %v", err, tc.wantErr)
			}
			// Fail-closed: nothing booked.
			if l.Version() != before {
				t.Errorf("version = %d, want %d (nothing booked)", l.Version(), before)
			}
			if n := l.MandateDebitCount(); n != 0 {
				t.Errorf("MandateDebitCount = %d, want 0", n)
			}
		})
	}

	// An unknown mandate is refused, never auto-created.
	t.Run("unknown mandate is not auto-created", func(t *testing.T) {
		l, _ := pacs003InboundFixture(t)
		if _, err := applyPacs003Doc(t, l, pacs003Doc("msg-n", "e2e-n", "10.00", "USD", "m-ghost", today, "")); !errors.Is(err, ErrMandateNotFound) {
			t.Fatalf("ApplyPacs003 err = %v, want ErrMandateNotFound", err)
		}
		if _, ok := l.GetMandate("m-ghost"); ok {
			t.Errorf("mandate m-ghost exists after refusal, want no auto-created mandate")
		}
	})
}

func TestApplyPacs003Replay(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pacs003InboundLedger(t, WithAuditLog(al))
	today := time.Now().UTC().Format("2006-01-02")
	doc := pacs003Doc("msg-replay", "e2e-replay", "100.00", "USD", "m-sub", today, "")
	first, err := applyPacs003Doc(t, l, doc)
	if err != nil {
		t.Fatalf("first ApplyPacs003: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first apply duplicate = true, want false")
	}
	versionAfterFirst := l.Version()

	// The same MsgId re-sent: the original receipt, no new booking.
	second, err := applyPacs003Doc(t, l, doc)
	if err != nil {
		t.Fatalf("replay ApplyPacs003: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("replay duplicate = false, want true")
	}
	if second.Entry.ID != first.Entry.ID {
		t.Errorf("replay entry = %q, want %q", second.Entry.ID, first.Entry.ID)
	}
	if l.Version() != versionAfterFirst {
		t.Errorf("version = %d, want %d (replay books nothing)", l.Version(), versionAfterFirst)
	}
	if n := l.MandateDebitCount(); n != 1 {
		t.Errorf("MandateDebitCount = %d, want 1", n)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000", got)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pacs003_debit_applied") != 1 {
		t.Errorf("pacs003_debit_applied count = %d, want 1 (no second event on replay)", countOp(events, "pacs003_debit_applied"))
	}

	// Even after the mandate is suspended, the replay still returns
	// the original receipt: the replay books nothing new, so the
	// mandate gates must not block it (the DebitWithMandate
	// replay-before-gates convention).
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	third, err := applyPacs003Doc(t, l, doc)
	if err != nil {
		t.Fatalf("replay after suspend: %v", err)
	}
	if !third.Duplicate || third.Entry.ID != first.Entry.ID {
		t.Errorf("replay after suspend = %+v, want duplicate of %q", third, first.Entry.ID)
	}
}
