package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt103Fields is one inbound MT103 test document: every field of
// the nine-field core set, so parse refusals can be driven per
// field.
type mt103Fields struct {
	senderLT        string
	receiverLT      string
	ref             string
	bankOp          string
	valueDate       string // YYMMDD
	currency        string
	amount          string // "<integer>,<2 fraction digits>"
	orderingIBAN    string
	orderingName    []string
	orderingBIC     string
	beneficiaryBIC  string
	beneficiaryIBAN string
	beneficiaryName []string
	remit           []string
	charges         string
}

// mt103Defaults is the happy-path document: cust pays merchant
// USD 100.00, value date today (UTC), CRED/SHA, with both
// institution BICs and one remittance line.
func mt103Defaults() mt103Fields {
	return mt103Fields{
		senderLT:        "DEUTDEFFXXX",
		receiverLT:      "NWBKGB2LXXX",
		ref:             "MT103-REF-01",
		bankOp:          "CRED",
		valueDate:       time.Now().UTC().Format("060102"),
		currency:        "USD",
		amount:          "100,00",
		orderingIBAN:    testIBANDE,
		orderingName:    []string{"Alice Customer"},
		orderingBIC:     "DEUTDEFF",
		beneficiaryBIC:  "NWBKGB2L",
		beneficiaryIBAN: testIBANGB,
		beneficiaryName: []string{"Bob Merchant"},
		remit:           []string{"invoice 42"},
		charges:         "SHA",
	}
}

// mt103Render renders f as a SWIFT FIN MT103 message: blocks 1–4,
// lines joined with "\n", no trailing newline. An empty
// orderingBIC/beneficiaryBIC omits the :52A:/:57A: field; an empty
// remit omits :70:.
func mt103Render(f mt103Fields) string {
	var sb strings.Builder
	sb.WriteString("{1:F01" + f.senderLT + "0000000000}\n")
	sb.WriteString("{2:I103" + f.receiverLT + "U3003}\n")
	sb.WriteString("{3:{108:" + f.ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + f.ref + "\n")
	sb.WriteString(":23B:" + f.bankOp + "\n")
	sb.WriteString(":32A:" + f.valueDate + f.currency + f.amount + "\n")
	sb.WriteString(":50K:/" + f.orderingIBAN + "\n")
	for _, ln := range f.orderingName {
		sb.WriteString(ln + "\n")
	}
	if f.orderingBIC != "" {
		sb.WriteString(":52A:" + f.orderingBIC + "\n")
	}
	if f.beneficiaryBIC != "" {
		sb.WriteString(":57A:" + f.beneficiaryBIC + "\n")
	}
	sb.WriteString(":59:/" + f.beneficiaryIBAN + "\n")
	for _, ln := range f.beneficiaryName {
		sb.WriteString(ln + "\n")
	}
	if len(f.remit) > 0 {
		sb.WriteString(":70:" + strings.Join(f.remit, "\n") + "\n")
	}
	sb.WriteString(":71A:" + f.charges + "\n")
	sb.WriteString("-}")
	return sb.String()
}

// mt103ApplyLedger builds a ledger with a funded payer and IBANs
// on both sides, ready for MT103 application through
// PostTransfer: cust holds USD 10,000.00, merchant holds nothing
// yet.
func mt103ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	return l
}

// applyMT103Doc parses doc and applies it with no explicit actor,
// so the audit event carries the ApplyMT103 default attribution
// the spec names, failing the test on parse errors: the gates
// under test are the apply gates, not the parse gates.
func applyMT103Doc(t *testing.T, l *Ledger, doc string) (MT103ApplyResult, error) {
	t.Helper()
	m, err := ParseMT103(doc)
	if err != nil {
		t.Fatalf("ParseMT103: %v", err)
	}
	return l.ApplyMT103(m, "")
}

func TestParseMT103_RoundTrip(t *testing.T) {
	f := mt103Defaults()
	m, err := ParseMT103(mt103Render(f))
	if err != nil {
		t.Fatalf("ParseMT103: %v", err)
	}
	if m.SenderLT != "DEUTDEFFXXX" || m.ReceiverLT != "NWBKGB2LXXX" {
		t.Errorf("LTs = %q/%q, want DEUTDEFFXXX/NWBKGB2LXXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != "MT103-REF-01" {
		t.Errorf(":20: = %q, want MT103-REF-01", m.Reference)
	}
	if m.BankOperationCode != "CRED" {
		t.Errorf(":23B: = %q, want CRED", m.BankOperationCode)
	}
	if mt103ValueDate(m.ValueDate) != time.Now().UTC().Format("060102") {
		t.Errorf(":32A: value date = %q, want today", mt103ValueDate(m.ValueDate))
	}
	if m.Currency != "USD" || m.AmountCents != 10000 {
		t.Errorf(":32A: = %s %d, want USD 10000", m.Currency, m.AmountCents)
	}
	if m.OrderingAccount != testIBANDE {
		t.Errorf(":50K: = %q, want %q", m.OrderingAccount, testIBANDE)
	}
	if len(m.OrderingName) != 1 || m.OrderingName[0] != "Alice Customer" {
		t.Errorf(":50K: names = %q, want [Alice Customer]", m.OrderingName)
	}
	if m.OrderingInstitutionBIC != "DEUTDEFF" {
		t.Errorf(":52A: = %q, want DEUTDEFF", m.OrderingInstitutionBIC)
	}
	if m.BeneficiaryInstitutionBIC != "NWBKGB2L" {
		t.Errorf(":57A: = %q, want NWBKGB2L", m.BeneficiaryInstitutionBIC)
	}
	if m.BeneficiaryAccount != testIBANGB {
		t.Errorf(":59: = %q, want %q", m.BeneficiaryAccount, testIBANGB)
	}
	if len(m.BeneficiaryName) != 1 || m.BeneficiaryName[0] != "Bob Merchant" {
		t.Errorf(":59: names = %q, want [Bob Merchant]", m.BeneficiaryName)
	}
	if m.RemittanceInfo != "invoice 42" {
		t.Errorf(":70: = %q, want invoice 42", m.RemittanceInfo)
	}
	if m.DetailsOfCharges != "SHA" {
		t.Errorf(":71A: = %q, want SHA", m.DetailsOfCharges)
	}
	// Optional fields omit cleanly: no :52A:/:57A:, no :70:, no
	// name lines.
	f.orderingBIC, f.beneficiaryBIC = "", ""
	f.orderingName, f.beneficiaryName, f.remit = nil, nil, nil
	m, err = ParseMT103(mt103Render(f))
	if err != nil {
		t.Fatalf("ParseMT103(minimal): %v", err)
	}
	if m.OrderingInstitutionBIC != "" || m.BeneficiaryInstitutionBIC != "" || m.RemittanceInfo != "" {
		t.Errorf("minimal: BICs/remit = %q/%q/%q, want empty", m.OrderingInstitutionBIC, m.BeneficiaryInstitutionBIC, m.RemittanceInfo)
	}
	if len(m.OrderingName) != 0 || len(m.BeneficiaryName) != 0 {
		t.Errorf("minimal: names = %q/%q, want empty", m.OrderingName, m.BeneficiaryName)
	}
}

func TestParseMT103_RejectsBadBlockStructure(t *testing.T) {
	good := mt103Render(mt103Defaults())
	today := time.Now().UTC().Format("060102")
	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT202", strings.Replace(good, "{2:I103", "{2:I202", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:MT103-REF-01}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"content after trailer", good + "\n:20:EXTRA"},
		{"field order swapped", strings.Replace(good, ":20:MT103-REF-01\n:23B:CRED\n", ":23B:CRED\n:20:MT103-REF-01\n", 1)},
		{"missing :32A:", strings.Replace(good, ":32A:"+today+"USD100,00\n", "", 1)},
		{"missing :71A:", strings.Replace(good, ":71A:SHA\n", "", 1)},
		{"missing :59:", strings.Replace(good, ":59:/"+testIBANGB+"\nBob Merchant\n", "", 1)},
		{"bad :32A: date", strings.Replace(good, ":32A:"+today, ":32A:991332", 1)},
		{"bad :32A: amount", strings.Replace(good, ":32A:"+today+"USD100,00", ":32A:"+today+"USD100.00", 1)},
		{"zero :32A: amount", strings.Replace(good, ":32A:"+today+"USD100,00", ":32A:"+today+"USD0,00", 1)},
		{"bad :32A: currency", strings.Replace(good, ":32A:"+today+"USD", ":32A:"+today+"usd", 1)},
		{"bad :23B: code", strings.Replace(good, ":23B:CRED", ":23B:FAST", 1)},
		{"bad :71A: code", strings.Replace(good, ":71A:SHA", ":71A:ALL", 1)},
		{"bad :52A: BIC", strings.Replace(good, ":52A:DEUTDEFF", ":52A:NOTABIC", 1)},
		{"non-IBAN :50K:", strings.Replace(good, ":50K:/"+testIBANDE, ":50K:/12345678", 1)},
		{"non-IBAN :59:", strings.Replace(good, ":59:/"+testIBANGB, ":59:/12345678", 1)},
		{"empty line in block 4", strings.Replace(good, ":23B:CRED\n", ":23B:CRED\n\n", 1)},
		{"too many :50K: name lines", strings.Replace(good, ":50K:/"+testIBANDE+"\nAlice Customer\n", ":50K:/"+testIBANDE+"\nAlice Customer\nLine Two\nLine Three\nLine Four\n", 1)},
		{"too many :70: lines", strings.Replace(good, ":70:invoice 42\n", ":70:one\ntwo\nthree\nfour\nfive\n", 1)},
		{"bad :20: reference", strings.Replace(good, ":20:MT103-REF-01", ":20:MT103-REF-01/EXTRA//DOUBLE", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT103(tc.doc); err == nil {
				t.Errorf("ParseMT103(%s): want error, got nil", tc.name)
			}
		})
	}
}

func TestParseMT103_RejectsUnknownField(t *testing.T) {
	good := mt103Render(mt103Defaults())
	cases := []struct {
		name string
		doc  string
	}{
		{"extra :72: field", strings.Replace(good, ":71A:SHA\n", ":72:/SOME INFO\n:71A:SHA\n", 1)},
		{"extra :77B: field", strings.Replace(good, ":71A:SHA\n", ":77B:REGULATORY\n:71A:SHA\n", 1)},
		{":52D: variant refused", strings.Replace(good, ":52A:DEUTDEFF\n", ":52D:Some Bank Name\n", 1)},
		{"field before block 4", strings.Replace(good, "{4:\n", ":20:EARLY\n{4:\n", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT103(tc.doc); err == nil {
				t.Errorf("ParseMT103(%s): want error, got nil", tc.name)
			}
		})
	}
}

func TestParseMT103_AcceptsCRLF(t *testing.T) {
	f := mt103Defaults()
	doc := mt103Render(f)
	crlf := strings.ReplaceAll(doc, "\n", "\r\n") + "\r\n"
	m, err := ParseMT103(crlf)
	if err != nil {
		t.Fatalf("ParseMT103(CRLF + trailing newline): %v", err)
	}
	if m.Reference != "MT103-REF-01" || m.AmountCents != 10000 || m.Currency != "USD" {
		t.Errorf("CRLF parse = %+v, want MT103-REF-01/USD 10000", m)
	}
	if m.RemittanceInfo != "invoice 42" {
		t.Errorf(":70: = %q, want invoice 42", m.RemittanceInfo)
	}
	// A lone LF trailing newline is tolerated too.
	m, err = ParseMT103(doc + "\n")
	if err != nil {
		t.Fatalf("ParseMT103(trailing newline): %v", err)
	}
	if m.Reference != "MT103-REF-01" {
		t.Errorf(":20: = %q, want MT103-REF-01", m.Reference)
	}
}

func TestApplyMT103_CreditsRegisteredIBAN(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt103ApplyLedger(t, al)
	res, err := applyMT103Doc(t, l, mt103Render(mt103Defaults()))
	if err != nil {
		t.Fatalf("ApplyMT103: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("ApplyMT103 result Err = %v, want nil", res.Err)
	}
	if res.RepairID != "" {
		t.Errorf("RepairID = %q, want empty", res.RepairID)
	}
	if res.From != "cust" || res.To != "merchant" {
		t.Errorf("legs = %q->%q, want cust->merchant", res.From, res.To)
	}
	if res.EntryID != "MT103-REF-01" {
		t.Errorf("EntryID = %q, want MT103-REF-01", res.EntryID)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	// The money moved: cust paid 100.00, merchant received it.
	if got := l.BalanceIn("cust", "USD"); got != 1000000-10000 {
		t.Errorf("cust balance = %d, want %d", got, 1000000-10000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000", got)
	}
	// Audit: one mt103_inbound_applied event with the exact
	// details — the :20: on the durable record, the BICs
	// recorded not routed.
	events := readAuditOps(t, al)
	if countOp(events, "mt103_inbound_applied") != 1 {
		t.Fatalf("mt103_inbound_applied count = %d, want 1", countOp(events, "mt103_inbound_applied"))
	}
	ev := findOp(events, "mt103_inbound_applied")
	if ev.Actor != "ApplyMT103" {
		t.Errorf("actor = %q, want ApplyMT103", ev.Actor)
	}
	if ev.Details["msg_id"] != "MT103-REF-01" {
		t.Errorf("msg_id = %v, want MT103-REF-01", ev.Details["msg_id"])
	}
	if ev.Details["amount_cents"] != float64(10000) {
		t.Errorf("amount_cents = %v, want 10000", ev.Details["amount_cents"])
	}
	if ev.Details["currency"] != "USD" {
		t.Errorf("currency = %v, want USD", ev.Details["currency"])
	}
	if ev.Details["ordering_institution_bic"] != "DEUTDEFF" {
		t.Errorf("ordering_institution_bic = %v, want DEUTDEFF", ev.Details["ordering_institution_bic"])
	}
	if ev.Details["beneficiary_institution_bic"] != "NWBKGB2L" {
		t.Errorf("beneficiary_institution_bic = %v, want NWBKGB2L", ev.Details["beneficiary_institution_bic"])
	}
}

func TestApplyMT103_UnregisteredIBANGoesToRepair(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt103ApplyLedger(t, al)
	// The beneficiary IBAN is well-formed but no account
	// registered it.
	f := mt103Defaults()
	f.beneficiaryIBAN = mt101UnknownIBAN
	res, err := applyMT103Doc(t, l, mt103Render(f))
	if err != nil {
		t.Fatalf("ApplyMT103: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT103 result Err = nil, want ErrMT103UnknownIBAN")
	}
	if !errors.Is(res.Err, ErrMT103UnknownIBAN) {
		t.Errorf("Err = %v, want ErrMT103UnknownIBAN", res.Err)
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if rec, ok := l.GetRepair(res.RepairID); !ok || rec.Status != RepairStatusOpen {
		t.Errorf("GetRepair(%q) = %+v/%v, want an open record", res.RepairID, rec, ok)
	}
	// Nothing booked: no entry, no money moved, no audit event.
	if res.EntryID != "" {
		t.Errorf("EntryID = %q, want empty", res.EntryID)
	}
	if got := l.BalanceIn("cust", "USD"); got != 1000000 {
		t.Errorf("cust balance = %d, want 1000000", got)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 0 {
		t.Errorf("merchant balance = %d, want 0", got)
	}
	// No account was auto-created from the wire IBAN.
	l.mu.RLock()
	_, resolves := l.accountByIBANLocked(mt101UnknownIBAN)
	l.mu.RUnlock()
	if resolves {
		t.Error("unregistered IBAN resolves after apply, want no auto-created account")
	}
	// The ordering side unregistered parks the same way (a
	// fresh :20:, so it is a new instruction, not a replay).
	f = mt103Defaults()
	f.ref = "MT103-REF-02"
	f.orderingIBAN = mt101UnknownIBAN
	res, err = applyMT103Doc(t, l, mt103Render(f))
	if err != nil {
		t.Fatalf("ApplyMT103(ordering unknown): %v", err)
	}
	if !errors.Is(res.Err, ErrMT103UnknownIBAN) || res.RepairID == "" {
		t.Errorf("ordering unknown: Err=%v RepairID=%q, want ErrMT103UnknownIBAN + repair", res.Err, res.RepairID)
	}
	// Nothing booked: no audit event for either parked
	// instruction (readAuditOps closes the log, so it runs
	// last, after both applies).
	if countOp(readAuditOps(t, al), "mt103_inbound_applied") != 0 {
		t.Error("mt103_inbound_applied emitted for a parked instruction, want none")
	}
}

func TestApplyMT103_RefusesFrozenPayer(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt103ApplyLedger(t, al)
	l.Freeze("cust")
	res, err := applyMT103Doc(t, l, mt103Render(mt103Defaults()))
	if err != nil {
		t.Fatalf("ApplyMT103: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT103 result Err = nil, want a frozen-payer refusal")
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if rec, ok := l.GetRepair(res.RepairID); !ok || rec.Status != RepairStatusOpen {
		t.Errorf("GetRepair(%q) = %+v/%v, want an open record", res.RepairID, rec, ok)
	}
	if got := l.BalanceIn("cust", "USD"); got != 1000000 {
		t.Errorf("cust balance = %d, want 1000000 (frozen payer moved nothing)", got)
	}
	if countOp(readAuditOps(t, al), "mt103_inbound_applied") != 0 {
		t.Error("mt103_inbound_applied emitted for a refused instruction, want none")
	}
}

func TestApplyMT103_RefusesFutureValueDate(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt103ApplyLedger(t, al)
	f := mt103Defaults()
	f.valueDate = time.Now().UTC().Add(24 * time.Hour).Format("060102")
	res, err := applyMT103Doc(t, l, mt103Render(f))
	if err != nil {
		t.Fatalf("ApplyMT103: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT103 result Err = nil, want ErrMT103ValueDateNotToday")
	}
	if !errors.Is(res.Err, ErrMT103ValueDateNotToday) {
		t.Errorf("Err = %v, want ErrMT103ValueDateNotToday", res.Err)
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if got := l.BalanceIn("cust", "USD"); got != 1000000 {
		t.Errorf("cust balance = %d, want 1000000 (future-dated instruction booked nothing)", got)
	}
	// A past date parks the same way: the ledger schedules
	// nothing, forward or backward.
	f.valueDate = time.Now().UTC().Add(-24 * time.Hour).Format("060102")
	f.ref = "MT103-REF-PAST"
	res, err = applyMT103Doc(t, l, mt103Render(f))
	if err != nil {
		t.Fatalf("ApplyMT103(past date): %v", err)
	}
	if !errors.Is(res.Err, ErrMT103ValueDateNotToday) || res.RepairID == "" {
		t.Errorf("past date: Err=%v RepairID=%q, want ErrMT103ValueDateNotToday + repair", res.Err, res.RepairID)
	}
	// No audit event for either parked instruction (readAuditOps
	// closes the log, so it runs last, after both applies).
	if countOp(readAuditOps(t, al), "mt103_inbound_applied") != 0 {
		t.Error("mt103_inbound_applied emitted for a future-dated instruction, want none")
	}
}

func TestApplyMT103_ReplaySameReferenceNoop(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt103ApplyLedger(t, al)
	doc := mt103Render(mt103Defaults())
	first, err := applyMT103Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT103: %v", err)
	}
	if first.Err != nil {
		t.Fatalf("first ApplyMT103 Err = %v", first.Err)
	}
	second, err := applyMT103Doc(t, l, doc)
	if err != nil {
		t.Fatalf("replay ApplyMT103: %v", err)
	}
	if second.Err != nil {
		t.Fatalf("replay Err = %v, want nil", second.Err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if second.EntryID != first.EntryID {
		t.Errorf("replay EntryID = %q, want %q", second.EntryID, first.EntryID)
	}
	// Nothing booked twice: the balances moved once, and the
	// audit event fired once.
	if got := l.BalanceIn("cust", "USD"); got != 1000000-10000 {
		t.Errorf("cust balance = %d, want %d", got, 1000000-10000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000", got)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt103_inbound_applied") != 1 {
		t.Errorf("mt103_inbound_applied count = %d, want 1", countOp(events, "mt103_inbound_applied"))
	}
}

func TestApplyMT103_SameReferenceDifferentTermsRefused(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt103ApplyLedger(t, al)
	if _, err := applyMT103Doc(t, l, mt103Render(mt103Defaults())); err != nil {
		t.Fatalf("ApplyMT103: %v", err)
	}
	// Same :20:, different amount: a conflict, never a silent
	// overwrite — and the original booking stands.
	f := mt103Defaults()
	f.amount = "200,00"
	_, err = applyMT103Doc(t, l, mt103Render(f))
	if err == nil {
		t.Fatal("ApplyMT103(different terms): want error, got nil")
	}
	if !errors.Is(err, ErrMT103MsgIDConflict) {
		t.Errorf("err = %v, want ErrMT103MsgIDConflict", err)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000 (conflict booked nothing)", got)
	}
	// Same :20:, different value date: a conflict too.
	f = mt103Defaults()
	f.valueDate = time.Now().UTC().Add(24 * time.Hour).Format("060102")
	if _, err := applyMT103Doc(t, l, mt103Render(f)); !errors.Is(err, ErrMT103MsgIDConflict) {
		t.Errorf("different value date: err = %v, want ErrMT103MsgIDConflict", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt103_inbound_applied") != 1 {
		t.Errorf("mt103_inbound_applied count = %d, want 1", countOp(events, "mt103_inbound_applied"))
	}
}
