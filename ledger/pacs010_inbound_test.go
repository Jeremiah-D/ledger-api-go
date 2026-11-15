package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pacs010InboundLedger creates a ledger with a funded payer and two
// active mandates whose validity windows open 48h ago: the inbound
// collection date is a UTC calendar date (midnight), so the window
// must already cover the start of today, unlike mandateFixture's
// now-minus-one-hour ValidFrom. The two mandates let a two-item batch
// book both items without tripping either mandate's per-day frequency
// window.
func pacs010InboundLedger(t *testing.T, opts ...Option) (*Ledger, Mandate, Mandate) {
	t.Helper()
	l := New(opts...)
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	m1, err := l.CreateMandate(Mandate{
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
		t.Fatalf("CreateMandate m-sub: %v", err)
	}
	m2, err := l.CreateMandate(Mandate{
		ID:             "m-sub2",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 500000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-48 * time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateMandate m-sub2: %v", err)
	}
	return l, m1, m2
}

func pacs010InboundFixture(t *testing.T) (*Ledger, Mandate, Mandate) {
	t.Helper()
	return pacs010InboundLedger(t)
}

// pacs010Item is one CdtInstr of a test document.
type pacs010Item struct {
	endToEnd  string
	amount    string
	ccy       string
	mandate   string
	colltnDt  string
	dtOfSgntr string
}

// pacs010Doc renders one inbound pacs.010.001.02 document: msgID in
// GrpHdr (SttlmMtd sttlmMtd, NbOfTxs and CtrlSum computed from the
// items), one CdtInstr per item pulling amount in ccy under
// mandateID, EndToEndId endToEnd, ReqdColltnDt colltnDt and
// DtOfSgntr dtOfSgntr.
func pacs010Doc(msgID, sttlmMtd string, items ...pacs010Item) string {
	return pacs010DocCtrl(msgID, sttlmMtd, "", -1, items...)
}

// pacs010TestCents parses a test amount tolerantly (zero, negative,
// over-long fraction) for the fixture's control-sum computation: the
// item under test is refused by the parse gate before the control
// sum is ever checked, so the fixture's sum only needs to be
// well-formed, not exact.
func pacs010TestCents(s string) int64 {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if len(fracPart) > 2 {
		fracPart = fracPart[:2]
	}
	whole, _ := strconv.ParseInt(intPart, 10, 64)
	var frac int64
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, _ = strconv.ParseInt(fracPart, 10, 64)
	}
	cents := whole*100 + frac
	if neg {
		cents = -cents
	}
	return cents
}

func pacs010TestAmount(cents int64) string {
	if cents < 0 {
		return "-" + pacs010TestAmount(-cents)
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// pacs010DocCtrl is pacs010Doc with an explicit CtrlSum (empty means
// computed) and NbOfTxs (negative means computed).
func pacs010DocCtrl(msgID, sttlmMtd, ctrlSum string, nbOfTxs int, items ...pacs010Item) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.010.001.02">`)
	sb.WriteString(`<FIDrctDbt><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T07:00:00Z</CreDtTm>`)
	if nbOfTxs < 0 {
		nbOfTxs = len(items)
	}
	sb.WriteString(fmt.Sprintf(`<NbOfTxs>%d</NbOfTxs>`, nbOfTxs))
	if ctrlSum == "" {
		var sum int64
		for _, it := range items {
			sum += pacs010TestCents(it.amount)
		}
		ctrlSum = pacs010TestAmount(sum)
	}
	sb.WriteString(`<CtrlSum>` + ctrlSum + `</CtrlSum>`)
	if sttlmMtd != "" {
		sb.WriteString(`<SttlmInf><SttlmMtd>` + sttlmMtd + `</SttlmMtd></SttlmInf>`)
	}
	sb.WriteString(`</GrpHdr>`)
	for _, it := range items {
		sb.WriteString(`<CdtInstr><PmtId><InstrId>instr-` + it.endToEnd + `</InstrId><EndToEndId>` + it.endToEnd + `</EndToEndId></PmtId>`)
		sb.WriteString(`<InstdAmt Ccy="` + it.ccy + `">` + it.amount + `</InstdAmt>`)
		if it.colltnDt != "" {
			sb.WriteString(`<ReqdColltnDt>` + it.colltnDt + `</ReqdColltnDt>`)
		}
		sb.WriteString(`<Cdtr><Nm>Merchant Bank</Nm></Cdtr>`)
		sb.WriteString(`<CdtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></CdtrAcct>`)
		sb.WriteString(`<CdtrAgt><FinInstnId><BIC>BANKCRED</BIC></FinInstnId></CdtrAgt>`)
		sb.WriteString(`<Dbtr><Nm>Customer Bank</Nm></Dbtr>`)
		sb.WriteString(`<DbtrAcct><Id><IBAN>DE89370400440532013001</IBAN></Id></DbtrAcct>`)
		sb.WriteString(`<DbtrAgt><FinInstnId><BIC>BANKDEBB</BIC></FinInstnId></DbtrAgt>`)
		sb.WriteString(`<DrctDbtTx><MndtRltdInf><MndtId>` + it.mandate + `</MndtId>`)
		if it.dtOfSgntr != "" {
			sb.WriteString(`<DtOfSgntr>` + it.dtOfSgntr + `</DtOfSgntr>`)
		}
		sb.WriteString(`</MndtRltdInf></DrctDbtTx>`)
		sb.WriteString(`<RmtInf><Ustrd>invoice 42</Ustrd></RmtInf>`)
		sb.WriteString(`</CdtInstr>`)
	}
	sb.WriteString(`</FIDrctDbt></Document>`)
	return sb.String()
}

func TestParsePacs010(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	doc, err := ParsePacs010([]byte(pacs010Doc("msg-1", "COVE",
		pacs010Item{endToEnd: "e2e-1", amount: "2500.00", ccy: "USD", mandate: "m-sub", colltnDt: today, dtOfSgntr: "2026-01-15"},
		pacs010Item{endToEnd: "e2e-2", amount: "10.50", ccy: "USD", mandate: "m-sub2", colltnDt: today, dtOfSgntr: "2026-01-15"},
	)))
	if err != nil {
		t.Fatalf("ParsePacs010: %v", err)
	}
	if doc.MsgID != "msg-1" || doc.SettlementMethod != "COVE" {
		t.Errorf("header = %+v, want msg-1/COVE", doc)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(doc.Items))
	}
	d := doc.Items[0]
	if d.MsgID != "msg-1" || d.InstrID != "instr-e2e-1" || d.EndToEndID != "e2e-1" {
		t.Errorf("identity = %+v, want msg-1/instr-e2e-1/e2e-1", d)
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
	if d.Creditor != "Merchant Bank" || d.Debtor != "Customer Bank" {
		t.Errorf("parties = %q/%q, want Merchant Bank/Customer Bank", d.Creditor, d.Debtor)
	}
	if d.CreditorAccount != "DE89370400440532013000" || d.DebtorAccount != "DE89370400440532013001" {
		t.Errorf("accounts = %q/%q, want the two stated IBANs", d.CreditorAccount, d.DebtorAccount)
	}
	if d.CreditorAgent != "BANKCRED" || d.DebtorAgent != "BANKDEBB" {
		t.Errorf("agents = %q/%q, want BANKCRED/BANKDEBB", d.CreditorAgent, d.DebtorAgent)
	}
	if d.Remittance != "invoice 42" {
		t.Errorf("remittance = %q, want invoice 42", d.Remittance)
	}
	if doc.Items[1].AmountCents != 1050 || doc.Items[1].EndToEndID != "e2e-2" {
		t.Errorf("second item = %+v, want e2e-2/1050", doc.Items[1])
	}
}

func TestParsePacs010Defaults(t *testing.T) {
	// No SttlmInf at all: the method reads as INDA, the export's
	// default. No ReqdColltnDt: the debit books at now.
	doc, err := ParsePacs010([]byte(pacs010Doc("msg-def", "",
		pacs010Item{endToEnd: "e2e-def", amount: "10.00", ccy: "USD", mandate: "m-sub"},
	)))
	if err != nil {
		t.Fatalf("ParsePacs010: %v", err)
	}
	if doc.SettlementMethod != "INDA" {
		t.Errorf("settlement method = %q, want INDA", doc.SettlementMethod)
	}
	if !doc.Items[0].CollectionDate.IsZero() {
		t.Errorf("collection date = %v, want zero (books at now)", doc.Items[0].CollectionDate)
	}
}

func TestParsePacs010Rejections(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	good := pacs010Item{endToEnd: "e2e-x", amount: "10.00", ccy: "USD", mandate: "m-sub", colltnDt: today, dtOfSgntr: "2026-01-15"}
	cases := []struct {
		name    string
		doc     string
		wantErr error
	}{
		{"malformed XML", `<Document><broken`, ErrPacs010Parse},
		{"empty document", `  `, ErrPacs010Parse},
		{"wrong namespace", strings.Replace(pacs010Doc("m", "INDA", good), "pacs.010.001.02", "pacs.010.001.03", 1), ErrPacs010Parse},
		{"missing MsgId", pacs010Doc("", "INDA", good), ErrPacs010Parse},
		{"no CdtInstr", pacs010Doc("m", "INDA"), ErrPacs010Parse},
		{"missing EndToEndId", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "", amount: "10.00", ccy: "USD", mandate: "m-sub", colltnDt: today}), ErrPacs010Parse},
		{"missing MndtId", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "10.00", ccy: "USD", mandate: "", colltnDt: today}), ErrPacs010Parse},
		{"zero amount", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "0.00", ccy: "USD", mandate: "m-sub", colltnDt: today}), ErrPacs010Parse},
		{"negative amount", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "-5.00", ccy: "USD", mandate: "m-sub", colltnDt: today}), ErrPacs010Parse},
		{"non-numeric amount", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "10.005", ccy: "USD", mandate: "m-sub", colltnDt: today}), ErrPacs010Parse},
		{"empty currency", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "10.00", ccy: "", mandate: "m-sub", colltnDt: today}), ErrPacs010Parse},
		{"lowercase currency", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "10.00", ccy: "usd", mandate: "m-sub", colltnDt: today}), ErrPacs010Parse},
		{"malformed signature date", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "10.00", ccy: "USD", mandate: "m-sub", colltnDt: today, dtOfSgntr: "15-01-2026"}), ErrPacs010Parse},
		{"malformed collection date", pacs010Doc("m", "INDA", pacs010Item{endToEnd: "e2e-x", amount: "10.00", ccy: "USD", mandate: "m-sub", colltnDt: "14/11/2026"}), ErrPacs010Parse},
		{"settlement method outside vocabulary", pacs010Doc("m", "XXXX", good), ErrPacs010InvalidSettlementMethod},
		{"control sum mismatch", pacs010DocCtrl("m", "INDA", "999.99", -1, good), ErrPacs010Parse},
		{"missing control sum", strings.Replace(pacs010Doc("m", "INDA", good), `<CtrlSum>10.00</CtrlSum>`, ``, 1), ErrPacs010Parse},
		{"NbOfTxs mismatch", pacs010DocCtrl("m", "INDA", "", 2, good), ErrPacs010Parse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePacs010([]byte(tc.doc)); !errors.Is(err, tc.wantErr) {
				t.Errorf("ParsePacs010 err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// applyPacs010Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyPacs010Doc(t *testing.T, l *Ledger, doc string) ([]Pacs010ItemResult, error) {
	t.Helper()
	parsed, err := ParsePacs010([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePacs010: %v", err)
	}
	return l.ApplyPacs010(parsed, "creditor-bank")
}

func TestApplyPacs010Success(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := pacs010InboundLedger(t, WithAuditLog(al))
	before := l.Version()
	today := time.Now().UTC().Format("2006-01-02")
	results, err := applyPacs010Doc(t, l, pacs010Doc("msg-ok", "INDA",
		pacs010Item{endToEnd: "e2e-ok-1", amount: "2500.00", ccy: "USD", mandate: "m-sub", colltnDt: today, dtOfSgntr: "2026-01-15"},
		pacs010Item{endToEnd: "e2e-ok-2", amount: "100.00", ccy: "USD", mandate: "m-sub2", colltnDt: today, dtOfSgntr: "2026-01-15"},
	))
	if err != nil {
		t.Fatalf("ApplyPacs010: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, res := range results {
		if res.Err != nil {
			t.Errorf("item %s err = %v, want nil", res.Instruction.EndToEndID, res.Err)
		}
		if res.Duplicate {
			t.Errorf("item %s duplicate = true, want false", res.Instruction.EndToEndID)
		}
	}
	r1 := results[0]
	if r1.Receipt.MandateID != "m-sub" {
		t.Errorf("mandate_id = %q, want m-sub", r1.Receipt.MandateID)
	}
	e := r1.Receipt.Entry
	// The entry ID is the instruction's EndToEndId; the legs come
	// from the mandate (debit payee, credit payer), not the document.
	if e.ID != "e2e-ok-1" {
		t.Errorf("entry ID = %q, want e2e-ok-1", e.ID)
	}
	if e.DebitAccount != "merchant" || e.CreditAccount != "cust" {
		t.Errorf("legs = %s/%s, want merchant/cust", e.DebitAccount, e.CreditAccount)
	}
	if e.AmountCents != 250000 || e.Currency != "USD" {
		t.Errorf("entry = %+v, want 250000 USD", e)
	}
	if e.Memo != "pacs010:msg-ok mandate_of:m-sub" {
		t.Errorf("memo = %q, want %q", e.Memo, "pacs010:msg-ok mandate_of:m-sub")
	}
	// The collection date is the booking timestamp.
	if got := e.CreatedAt.UTC().Format("2006-01-02"); got != today {
		t.Errorf("entry CreatedAt = %q, want %q", got, today)
	}
	if results[1].Receipt.Entry.ID != "e2e-ok-2" || results[1].Receipt.Entry.AmountCents != 10000 {
		t.Errorf("second receipt = %+v, want e2e-ok-2/10000", results[1].Receipt)
	}
	if l.Version() != before+2 {
		t.Errorf("version = %d, want %d", l.Version(), before+2)
	}
	if got := l.BalanceIn("cust", "USD"); got != 1000000-250000-10000 {
		t.Errorf("cust balance = %d, want %d", got, 1000000-250000-10000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 260000 {
		t.Errorf("merchant balance = %d, want %d", got, 260000)
	}
	if n := l.MandateDebitCount(); n != 2 {
		t.Errorf("MandateDebitCount = %d, want 2", n)
	}
	// Audit: one inbound marker per fresh booking, plus the
	// mandate_debit event DebitWithMandate emits per booking.
	events := readAuditOps(t, al)
	if countOp(events, "pacs010_debit_applied") != 2 {
		t.Errorf("pacs010_debit_applied count = %d, want 2", countOp(events, "pacs010_debit_applied"))
	}
	if countOp(events, "mandate_debit") != 2 {
		t.Errorf("mandate_debit count = %d, want 2", countOp(events, "mandate_debit"))
	}
	if countOp(events, "pacs010_debit_rejected") != 0 {
		t.Errorf("pacs010_debit_rejected count = %d, want 0", countOp(events, "pacs010_debit_rejected"))
	}
	ev := findOp(events, "pacs010_debit_applied")
	if ev == nil {
		t.Fatalf("pacs010_debit_applied event missing")
	}
	if ev.TraceID != "msg-ok" {
		t.Errorf("trace = %q, want msg-ok", ev.TraceID)
	}
}

func TestApplyPacs010Gates(t *testing.T) {
	now := time.Now()
	today := now.UTC().Format("2006-01-02")
	cases := []struct {
		name    string
		setup   func(t *testing.T, l *Ledger)
		item    pacs010Item
		wantErr error
	}{
		{
			"unknown mandate",
			func(t *testing.T, l *Ledger) {},
			pacs010Item{endToEnd: "e2e-u", amount: "10.00", ccy: "USD", mandate: "m-no-such", colltnDt: today},
			ErrMandateNotFound,
		},
		{
			"suspended mandate",
			func(t *testing.T, l *Ledger) {
				if _, err := l.SuspendMandate("m-sub"); err != nil {
					t.Fatalf("SuspendMandate: %v", err)
				}
			},
			pacs010Item{endToEnd: "e2e-s", amount: "10.00", ccy: "USD", mandate: "m-sub", colltnDt: today},
			ErrMandateNotActive,
		},
		{
			"collection after ValidUntil",
			func(t *testing.T, l *Ledger) {},
			pacs010Item{endToEnd: "e2e-f", amount: "10.00", ccy: "USD", mandate: "m-sub", colltnDt: now.Add(31 * 24 * time.Hour).UTC().Format("2006-01-02")},
			ErrMandateExpired,
		},
		{
			"collection before ValidFrom",
			func(t *testing.T, l *Ledger) {},
			pacs010Item{endToEnd: "e2e-p", amount: "10.00", ccy: "USD", mandate: "m-sub", colltnDt: now.Add(-72 * time.Hour).UTC().Format("2006-01-02")},
			ErrMandateNotActive,
		},
		{
			"amount over cap",
			func(t *testing.T, l *Ledger) {},
			pacs010Item{endToEnd: "e2e-o", amount: "5000.01", ccy: "USD", mandate: "m-sub", colltnDt: today},
			ErrMandateAmountExceeded,
		},
		{
			"currency mismatch",
			func(t *testing.T, l *Ledger) {},
			pacs010Item{endToEnd: "e2e-c", amount: "10.00", ccy: "EUR", mandate: "m-sub", colltnDt: today},
			ErrMandateCurrencyMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _, _ := pacs010InboundFixture(t)
			tc.setup(t, l)
			before := l.Version()
			results, err := applyPacs010Doc(t, l, pacs010Doc("msg-gate", "INDA", tc.item))
			if err != nil {
				t.Fatalf("ApplyPacs010 top-level err = %v, want nil (per-item failure)", err)
			}
			if len(results) != 1 {
				t.Fatalf("results = %d, want 1", len(results))
			}
			if !errors.Is(results[0].Err, tc.wantErr) {
				t.Fatalf("item err = %v, want %v", results[0].Err, tc.wantErr)
			}
			if results[0].EntryID != "" {
				t.Errorf("entry_id = %q, want empty on rejection", results[0].EntryID)
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
		l, _, _ := pacs010InboundFixture(t)
		results, err := applyPacs010Doc(t, l, pacs010Doc("msg-n", "INDA",
			pacs010Item{endToEnd: "e2e-n", amount: "10.00", ccy: "USD", mandate: "m-ghost", colltnDt: today},
		))
		if err != nil {
			t.Fatalf("ApplyPacs010 top-level err = %v, want nil", err)
		}
		if !errors.Is(results[0].Err, ErrMandateNotFound) {
			t.Fatalf("item err = %v, want ErrMandateNotFound", results[0].Err)
		}
		if _, ok := l.GetMandate("m-ghost"); ok {
			t.Errorf("mandate m-ghost exists after refusal, want no auto-created mandate")
		}
	})

	// One bad instruction never blocks the others: a mixed batch
	// books the covered item and rejects the rest, each with an
	// explicit rejection audit event.
	t.Run("one bad item does not block the batch", func(t *testing.T) {
		al := mustAuditLog(t)
		l, _, _ := pacs010InboundLedger(t, WithAuditLog(al))
		before := l.Version()
		results, err := applyPacs010Doc(t, l, pacs010Doc("msg-mix", "CLRG",
			pacs010Item{endToEnd: "e2e-mix-ok", amount: "100.00", ccy: "USD", mandate: "m-sub", colltnDt: today},
			pacs010Item{endToEnd: "e2e-mix-unknown", amount: "10.00", ccy: "USD", mandate: "m-no-such", colltnDt: today},
			pacs010Item{endToEnd: "e2e-mix-over", amount: "6000.00", ccy: "USD", mandate: "m-sub2", colltnDt: today},
		))
		if err != nil {
			t.Fatalf("ApplyPacs010 top-level err = %v, want nil", err)
		}
		if len(results) != 3 {
			t.Fatalf("results = %d, want 3", len(results))
		}
		if results[0].Err != nil || results[0].Receipt.Entry.ID != "e2e-mix-ok" {
			t.Errorf("first result = %+v, want booked e2e-mix-ok", results[0])
		}
		if !errors.Is(results[1].Err, ErrMandateNotFound) {
			t.Errorf("second item err = %v, want ErrMandateNotFound", results[1].Err)
		}
		if !errors.Is(results[2].Err, ErrMandateAmountExceeded) {
			t.Errorf("third item err = %v, want ErrMandateAmountExceeded", results[2].Err)
		}
		if l.Version() != before+1 {
			t.Errorf("version = %d, want %d (one booking)", l.Version(), before+1)
		}
		if n := l.MandateDebitCount(); n != 1 {
			t.Errorf("MandateDebitCount = %d, want 1", n)
		}
		events := readAuditOps(t, al)
		if countOp(events, "pacs010_debit_applied") != 1 {
			t.Errorf("pacs010_debit_applied count = %d, want 1", countOp(events, "pacs010_debit_applied"))
		}
		if countOp(events, "pacs010_debit_rejected") != 2 {
			t.Errorf("pacs010_debit_rejected count = %d, want 2", countOp(events, "pacs010_debit_rejected"))
		}
	})
}

func TestApplyPacs010Replay(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := pacs010InboundLedger(t, WithAuditLog(al))
	today := time.Now().UTC().Format("2006-01-02")
	doc := pacs010Doc("msg-replay", "INDA",
		pacs010Item{endToEnd: "e2e-replay", amount: "100.00", ccy: "USD", mandate: "m-sub", colltnDt: today},
	)
	first, err := applyPacs010Doc(t, l, doc)
	if err != nil {
		t.Fatalf("first ApplyPacs010: %v", err)
	}
	if first[0].Duplicate {
		t.Fatalf("first apply duplicate = true, want false")
	}
	versionAfterFirst := l.Version()

	// The same MsgId re-sent: the original receipt, no new booking.
	second, err := applyPacs010Doc(t, l, doc)
	if err != nil {
		t.Fatalf("replay ApplyPacs010: %v", err)
	}
	if !second[0].Duplicate {
		t.Errorf("replay duplicate = false, want true")
	}
	if second[0].EntryID != first[0].EntryID {
		t.Errorf("replay entry = %q, want %q", second[0].EntryID, first[0].EntryID)
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
	if countOp(events, "pacs010_debit_applied") != 1 {
		t.Errorf("pacs010_debit_applied count = %d, want 1 (no second event on replay)", countOp(events, "pacs010_debit_applied"))
	}

	// Even after the mandate is suspended, the replay still returns
	// the original receipt: the replay books nothing new, so the
	// mandate gates must not block it (the DebitWithMandate
	// replay-before-gates convention).
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	third, err := applyPacs010Doc(t, l, doc)
	if err != nil {
		t.Fatalf("replay after suspend: %v", err)
	}
	if !third[0].Duplicate || third[0].EntryID != first[0].EntryID {
		t.Errorf("replay after suspend = %+v, want duplicate of %q", third[0], first[0].EntryID)
	}
}
