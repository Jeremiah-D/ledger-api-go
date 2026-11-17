package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mt304ApplyLedger builds a ledger with registered BICs on both
// parties, ready for inbound MT304 instructions. No forward is
// pre-booked: an INST instruction books its own.
func mt304ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	l := New(WithFXAccount("fx"), WithAuditLog(al))
	if err := l.SetAccountIBAN("payer", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN payer: %v", err)
	}
	if err := l.SetAccountIBAN("payee", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN payee: %v", err)
	}
	return l
}

// mt304GoodDoc renders the canonical inbound MT304 instruction:
// fund and manager are the payer (DEUTDEFF), the broker is the
// payee (NWBKGB2L), the delivery and receiving banks are the
// broker, the custodian is CITIUS33 — the same parties the LG-390
// export renders for the payer/payee forward.
func mt304GoodDoc(ref, scope string, tradeDate, valueDate time.Time) string {
	return fmt.Sprintf("{1:F01DEUTDEFFXXX0000000000}\n"+
		"{2:I304CITIUS33XXXU3003}\n"+
		"{3:{108:%s}}\n"+
		"{4:\n"+
		":15A:\n"+
		":20:%s\n"+
		":22A:NEWT\n"+
		":94A:%s\n"+
		":83A:DEUTDEFF\n"+
		":82A:DEUTDEFF\n"+
		":87A:NWBKGB2L\n"+
		":15B:\n"+
		":30T:%s\n"+
		":30V:%s\n"+
		":36:1,1\n"+
		":32B:EUR110,00\n"+
		":53A:NWBKGB2L\n"+
		":33B:USD100,00\n"+
		":57A:NWBKGB2L\n"+
		"-}", ref, ref, scope, mt940ValueDate(tradeDate), mt940ValueDate(valueDate))
}

// mt304FutureDates returns a trade date of today and a value date
// 48 hours out, so an INST instruction books a forward whose
// SettleAt is in the future.
func mt304FutureDates() (tradeDate, valueDate time.Time) {
	now := time.Now().UTC()
	return now, now.Add(48 * time.Hour)
}

// applyMT304Doc parses doc and applies it with no explicit actor,
// so the audit event carries the ApplyMT304 default attribution
// the spec names, failing the test on parse errors: the gates
// under test are the apply gates, not the parse gates.
func applyMT304Doc(t *testing.T, l *Ledger, doc string) (MT304ApplyResult, error) {
	t.Helper()
	m, err := ParseMT304(doc)
	if err != nil {
		t.Fatalf("ParseMT304: %v", err)
	}
	return l.ApplyMT304(m, "")
}

func TestParseMT304_RoundTrip(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-2001")
	doc, err := l.ExportMT304(MT304ExportOptions{ForwardID: c.ID, CustodianBIC: "CITIUS33", Scope: "INST"})
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}
	m, err := ParseMT304(doc)
	if err != nil {
		t.Fatalf("ParseMT304(export): %v", err)
	}
	if m.SenderLT != "DEUTDEFFXXX" || m.ReceiverLT != "CITIUS33XXX" {
		t.Errorf("LTs = %q/%q, want DEUTDEFFXXX/CITIUS33XXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != c.ID {
		t.Errorf(":20: = %q, want %q", m.Reference, c.ID)
	}
	if m.Variant != "NEWT" {
		t.Errorf(":22A: = %q, want NEWT", m.Variant)
	}
	if m.Scope != "INST" {
		t.Errorf(":94A: = %q, want INST", m.Scope)
	}
	if m.FundParty != "DEUTDEFF" || !m.FundPartyBIC {
		t.Errorf(":83a: = %q (BIC=%v), want DEUTDEFF (BIC)", m.FundParty, m.FundPartyBIC)
	}
	if m.ManagerParty != "DEUTDEFF" || !m.ManagerPartyBIC {
		t.Errorf(":82a: = %q (BIC=%v), want DEUTDEFF (BIC)", m.ManagerParty, m.ManagerPartyBIC)
	}
	if m.BrokerParty != "NWBKGB2L" || !m.BrokerPartyBIC {
		t.Errorf(":87a: = %q (BIC=%v), want NWBKGB2L (BIC)", m.BrokerParty, m.BrokerPartyBIC)
	}
	if mt103ValueDate(m.TradeDate) != mt103ValueDate(c.BookedAt) {
		t.Errorf(":30T: = %q, want %q", mt103ValueDate(m.TradeDate), mt103ValueDate(c.BookedAt))
	}
	if mt103ValueDate(m.ValueDate) != mt103ValueDate(c.SettleAt) {
		t.Errorf(":30V: = %q, want %q", mt103ValueDate(m.ValueDate), mt103ValueDate(c.SettleAt))
	}
	if m.Rate != "1,1" {
		t.Errorf(":36: = %q, want 1,1", m.Rate)
	}
	if m.BoughtCurrency != "EUR" || m.BoughtCents != 11000 {
		t.Errorf(":32B: = %s %d, want EUR 11000", m.BoughtCurrency, m.BoughtCents)
	}
	if m.DeliveryParty != "NWBKGB2L" || !m.DeliveryPartyBIC {
		t.Errorf(":53a: = %q (BIC=%v), want NWBKGB2L (BIC)", m.DeliveryParty, m.DeliveryPartyBIC)
	}
	if m.SoldCurrency != "USD" || m.SoldCents != 10000 {
		t.Errorf(":33B: = %s %d, want USD 10000", m.SoldCurrency, m.SoldCents)
	}
	if m.AccountParty != "NWBKGB2L" || !m.AccountPartyBIC {
		t.Errorf(":57a: = %q (BIC=%v), want NWBKGB2L (BIC)", m.AccountParty, m.AccountPartyBIC)
	}
	// The ADVI export round-trips its scope too.
	advi, err := l.ExportMT304(MT304ExportOptions{ForwardID: c.ID, CustodianBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT304(ADVI): %v", err)
	}
	am, err := ParseMT304(advi)
	if err != nil {
		t.Fatalf("ParseMT304(ADVI export): %v", err)
	}
	if am.Scope != "ADVI" {
		t.Errorf("ADVI :94A: = %q, want ADVI", am.Scope)
	}
}

func TestParseMT304_RejectsBadBlockStructure(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-2001")
	good, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT300", strings.Replace(good, "{2:I304", "{2:I300", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:fwd-mt304-2001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":30T:"+mt940ValueDate(c.BookedAt)+"\n:30V:"+mt940ValueDate(c.SettleAt)+"\n",
			":30V:"+mt940ValueDate(c.SettleAt)+"\n:30T:"+mt940ValueDate(c.BookedAt)+"\n", 1)},
		{"missing :15B:", strings.Replace(good, ":15B:\n", "", 1)},
		{"missing :36:", strings.Replace(good, ":36:1,1\n", "", 1)},
		{"missing :94A:", strings.Replace(good, ":94A:ADVI\n", "", 1)},
		{"bad :30V: date", strings.Replace(good, ":30V:"+mt940ValueDate(c.SettleAt), ":30V:991332", 1)},
		{"bad :36: rate", strings.Replace(good, ":36:1,1", ":36:1.1", 1)},
		{"bad :32B: amount", strings.Replace(good, ":32B:EUR110,00", ":32B:EUR110.00", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT304(tc.doc); err == nil {
				t.Errorf("ParseMT304(%s): want error, got nil", tc.name)
			}
		})
	}
}

func TestParseMT304_RejectsUnknownField(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-2001")
	good, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}

	cases := []struct {
		name string
		doc  string
	}{
		// An extra field between :33B: and :57A: — the line
		// count grows past 20, so the framing fails first.
		{"extra :72: field", strings.Replace(good, ":57A:NWBKGB2L", ":72:EXTRA\n:57A:NWBKGB2L", 1)},
		// A field tag the core set does not know, in place of
		// :57A:.
		{":72: instead of :57A:", strings.Replace(good, ":57A:NWBKGB2L", ":72:EXTRA", 1)},
		// An unknown party option letter is not a known form.
		{":83C: option", strings.Replace(good, ":83A:DEUTDEFF", ":83C:DEUTDEFF", 1)},
		// A field smuggled outside block 4, after the trailer.
		{"field after trailer", good + "\n:72:EXTRA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT304(tc.doc); err == nil {
				t.Errorf("ParseMT304(%s): want error, got nil", tc.name)
			}
		})
	}
}

func TestParseMT304_AcceptsCRLF(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-2001")
	doc, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}
	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	m, err := ParseMT304(crlf)
	if err != nil {
		t.Fatalf("ParseMT304(CRLF): %v", err)
	}
	if m.Reference != c.ID {
		t.Errorf(":20: = %q, want %q", m.Reference, c.ID)
	}
	// A single trailing newline after the `-}` trailer is
	// tolerated, like ParseMT300.
	m, err = ParseMT304(doc + "\n")
	if err != nil {
		t.Fatalf("ParseMT304(trailing newline): %v", err)
	}
	if m.Reference != c.ID {
		t.Errorf(":20: = %q, want %q", m.Reference, c.ID)
	}
}

func TestParseMT304_RejectsNonNewtVariant(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-2001")
	doc, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}
	for _, variant := range []string{"AMND", "CANC"} {
		t.Run(variant, func(t *testing.T) {
			bad := strings.Replace(doc, ":22A:NEWT", ":22A:"+variant, 1)
			if _, err := ParseMT304(bad); err == nil {
				t.Errorf("ParseMT304(:22A:%s): want error, got nil", variant)
			}
		})
	}
}

func TestParseMT304_RejectsIllegalScope(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-2001")
	doc, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}
	for _, scope := range []string{"BOGUS", "ADVS", ""} {
		t.Run("scope="+scope, func(t *testing.T) {
			bad := strings.Replace(doc, ":94A:ADVI", ":94A:"+scope, 1)
			if _, err := ParseMT304(bad); !errors.Is(err, ErrMT304Scope) {
				t.Errorf("ParseMT304(:94A:%s): err = %v, want ErrMT304Scope", scope, err)
			}
		})
	}
}

func TestApplyMT304INST_BooksForward(t *testing.T) {
	al := mustAuditLog(t)
	l := mt304ApplyLedger(t, al)
	tradeDate, valueDate := mt304FutureDates()
	doc := mt304GoodDoc("fwd-mt304-2002", "INST", tradeDate, valueDate)
	if _, ok := l.Forward("fwd-mt304-2002"); ok {
		t.Fatal("forward exists before apply, want none (INST books it)")
	}
	res, err := applyMT304Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT304: %v", err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	if res.Scope != "INST" {
		t.Errorf("Scope = %q, want INST", res.Scope)
	}
	fwd := res.Forward
	if fwd == nil {
		t.Fatal("Forward = nil, want the booked contract")
	}
	// The booked terms, exactly: parties, currencies, rate,
	// notional, value date, side.
	if fwd.ID != "fwd-mt304-2002" {
		t.Errorf("Forward ID = %q, want fwd-mt304-2002", fwd.ID)
	}
	if fwd.FromAccount != "payer" || fwd.ToAccount != "payee" {
		t.Errorf("Forward accounts = %q/%q, want payer/payee", fwd.FromAccount, fwd.ToAccount)
	}
	if fwd.FromCurrency != "USD" || fwd.ToCurrency != "EUR" {
		t.Errorf("Forward currencies = %q/%q, want USD/EUR", fwd.FromCurrency, fwd.ToCurrency)
	}
	if fwd.RateNum != 11 || fwd.RateDen != 10 {
		t.Errorf("Forward rate = %d/%d, want 11/10", fwd.RateNum, fwd.RateDen)
	}
	if fwd.NotionalCents != 10000 {
		t.Errorf("Forward notional = %d, want 10000", fwd.NotionalCents)
	}
	if mt103ValueDate(fwd.SettleAt) != mt103ValueDate(valueDate) {
		t.Errorf("Forward SettleAt = %q, want %q", mt103ValueDate(fwd.SettleAt), mt103ValueDate(valueDate))
	}
	if fwd.FXSide != "sell" {
		t.Errorf("Forward FXSide = %q, want sell (the MT304 default)", fwd.FXSide)
	}
	if fwd.Status != ForwardBooked {
		t.Errorf("Forward status = %q, want %q", fwd.Status, ForwardBooked)
	}
	// The stored contract reads back with the same terms.
	stored, ok := l.Forward("fwd-mt304-2002")
	if !ok || stored.ID != fwd.ID || stored.RateNum != 11 || stored.NotionalCents != 10000 {
		t.Errorf("Forward() = %+v/%v, want the booked contract", stored, ok)
	}
	// The audit trail carries exactly one deal-booked event,
	// with the specified details and actor — on top of
	// BookForward's own fx_forward_booked event.
	events := readAuditOps(t, al)
	booked := findOp(events, "mt304_deal_booked")
	if booked == nil {
		t.Fatalf("no mt304_deal_booked event in %d events", len(events))
	}
	if booked.Actor != "ApplyMT304" {
		t.Errorf("audit actor = %q, want ApplyMT304", booked.Actor)
	}
	if booked.Details["msg_id"] != "fwd-mt304-2002" {
		t.Errorf("audit msg_id = %v, want fwd-mt304-2002", booked.Details["msg_id"])
	}
	if booked.Details["forward_id"] != "fwd-mt304-2002" {
		t.Errorf("audit forward_id = %v, want fwd-mt304-2002", booked.Details["forward_id"])
	}
	if booked.Details["rate"] != "1,1" {
		t.Errorf("audit rate = %v, want 1,1", booked.Details["rate"])
	}
	if booked.Details["sold_cents"] != float64(10000) {
		t.Errorf("audit sold_cents = %v, want 10000", booked.Details["sold_cents"])
	}
	if booked.Details["bought_cents"] != float64(11000) {
		t.Errorf("audit bought_cents = %v, want 11000", booked.Details["bought_cents"])
	}
	if countOp(events, "mt304_deal_booked") != 1 {
		t.Errorf("mt304_deal_booked count = %d, want 1", countOp(events, "mt304_deal_booked"))
	}
	if countOp(events, "fx_forward_booked") != 1 {
		t.Errorf("fx_forward_booked count = %d, want 1", countOp(events, "fx_forward_booked"))
	}
}

func TestApplyMT304ADVI_RecordsAdviceOnly(t *testing.T) {
	al := mustAuditLog(t)
	l := mt304ApplyLedger(t, al)
	tradeDate, valueDate := mt304FutureDates()
	doc := mt304GoodDoc("fwd-mt304-2003", "ADVI", tradeDate, valueDate)
	res, err := applyMT304Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT304: %v", err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	if res.Scope != "ADVI" {
		t.Errorf("Scope = %q, want ADVI", res.Scope)
	}
	if res.Forward != nil {
		t.Errorf("Forward = %+v, want nil (ADVI books nothing)", res.Forward)
	}
	// No forward was booked: the forwards table has no new row.
	if _, ok := l.Forward("fwd-mt304-2003"); ok {
		t.Error("Forward exists after ADVI apply, want none")
	}
	// The advice is recorded and retrievable under its :20:.
	advice, ok := l.MT304Advice("fwd-mt304-2003")
	if !ok {
		t.Fatal("MT304Advice: want the recorded advice, got none")
	}
	if advice.Scope != "ADVI" || advice.Reference != "fwd-mt304-2003" {
		t.Errorf("advice = scope %q ref %q, want ADVI/fwd-mt304-2003", advice.Scope, advice.Reference)
	}
	if advice.BrokerParty != "NWBKGB2L" || advice.BoughtCents != 11000 || advice.SoldCents != 10000 {
		t.Errorf("advice terms = broker %q bought %d sold %d, want NWBKGB2L/11000/10000",
			advice.BrokerParty, advice.BoughtCents, advice.SoldCents)
	}
	if _, ok := l.MT304Advice("fwd-mt304-9999"); ok {
		t.Error("MT304Advice(unknown): want none, got a record")
	}
	// The audit trail carries exactly one advice event and no
	// booking event of either kind.
	events := readAuditOps(t, al)
	recorded := findOp(events, "mt304_advice_recorded")
	if recorded == nil {
		t.Fatalf("no mt304_advice_recorded event in %d events", len(events))
	}
	if recorded.Actor != "ApplyMT304" {
		t.Errorf("audit actor = %q, want ApplyMT304", recorded.Actor)
	}
	if recorded.Details["msg_id"] != "fwd-mt304-2003" {
		t.Errorf("audit msg_id = %v, want fwd-mt304-2003", recorded.Details["msg_id"])
	}
	if recorded.Details["scope"] != "ADVI" {
		t.Errorf("audit scope = %v, want ADVI", recorded.Details["scope"])
	}
	if countOp(events, "mt304_advice_recorded") != 1 {
		t.Errorf("mt304_advice_recorded count = %d, want 1", countOp(events, "mt304_advice_recorded"))
	}
	if countOp(events, "mt304_deal_booked") != 0 {
		t.Errorf("mt304_deal_booked count = %d, want 0", countOp(events, "mt304_deal_booked"))
	}
	if countOp(events, "fx_forward_booked") != 0 {
		t.Errorf("fx_forward_booked count = %d, want 0", countOp(events, "fx_forward_booked"))
	}
}

func TestApplyMT304_RefusesUnknownAccount(t *testing.T) {
	l := mt304ApplyLedger(t, mustAuditLog(t))
	tradeDate, valueDate := mt304FutureDates()
	cases := []struct {
		name string
		doc  string
	}{
		{"unknown fund BIC", strings.Replace(
			mt304GoodDoc("fwd-mt304-2004", "INST", tradeDate, valueDate),
			":83A:DEUTDEFF", ":83A:ZZZZUS33", 1)},
		{"unknown broker BIC", strings.Replace(
			mt304GoodDoc("fwd-mt304-2005", "INST", tradeDate, valueDate),
			":87A:NWBKGB2L", ":87A:ZZZZUS33", 1)},
		{"unknown broker BIC on ADVI", strings.Replace(
			mt304GoodDoc("fwd-mt304-2006", "ADVI", tradeDate, valueDate),
			":87A:NWBKGB2L", ":87A:ZZZZUS33", 1)},
		{"unknown D-form fund account", strings.Replace(
			mt304GoodDoc("fwd-mt304-2007", "INST", tradeDate, valueDate),
			":83A:DEUTDEFF", ":83D:NOSUCHACCT01", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMT304(tc.doc)
			if err != nil {
				t.Fatalf("ParseMT304: %v", err)
			}
			if _, err := l.ApplyMT304(m, ""); !errors.Is(err, ErrMT304UnknownAccount) {
				t.Errorf("ApplyMT304(%s): err = %v, want ErrMT304UnknownAccount", tc.name, err)
			}
		})
	}
}

func TestApplyMT304_RefusesPartyMismatch(t *testing.T) {
	l := mt304ApplyLedger(t, mustAuditLog(t))
	tradeDate, valueDate := mt304FutureDates()
	cases := []struct {
		name string
		doc  string
	}{
		{"manager is not the fund", strings.Replace(
			mt304GoodDoc("fwd-mt304-2008", "INST", tradeDate, valueDate),
			":82A:DEUTDEFF", ":82A:NWBKGB2L", 1)},
		{"delivery bank is not the broker", strings.Replace(
			mt304GoodDoc("fwd-mt304-2009", "INST", tradeDate, valueDate),
			":53A:NWBKGB2L", ":53A:DEUTDEFF", 1)},
		{"receiving bank is not the broker", strings.Replace(
			mt304GoodDoc("fwd-mt304-2010", "INST", tradeDate, valueDate),
			":57A:NWBKGB2L", ":57A:DEUTDEFF", 1)},
		{"fund and broker are the same account", strings.Replace(
			strings.Replace(
				mt304GoodDoc("fwd-mt304-2011", "INST", tradeDate, valueDate),
				":83A:DEUTDEFF", ":83A:NWBKGB2L", 1),
			":82A:DEUTDEFF", ":82A:NWBKGB2L", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMT304(tc.doc)
			if err != nil {
				t.Fatalf("ParseMT304: %v", err)
			}
			if _, err := l.ApplyMT304(m, ""); !errors.Is(err, ErrMT304TermsMismatch) {
				t.Errorf("ApplyMT304(%s): err = %v, want ErrMT304TermsMismatch", tc.name, err)
			}
		})
	}
}

func TestApplyMT304_RefusesBoughtAmountMismatch(t *testing.T) {
	l := mt304ApplyLedger(t, mustAuditLog(t))
	tradeDate, valueDate := mt304FutureDates()
	cases := []struct {
		name string
		doc  string
	}{
		{"bought amount off by one cent", strings.Replace(
			mt304GoodDoc("fwd-mt304-2012", "INST", tradeDate, valueDate),
			":32B:EUR110,00", ":32B:EUR110,01", 1)},
		{"bought currency wrong", strings.Replace(
			mt304GoodDoc("fwd-mt304-2013", "INST", tradeDate, valueDate),
			":32B:EUR110,00", ":32B:USD110,00", 1)},
		{"sold amount off by one cent", strings.Replace(
			mt304GoodDoc("fwd-mt304-2014", "INST", tradeDate, valueDate),
			":33B:USD100,00", ":33B:USD100,01", 1)},
		// floor(10000 * 1/3) = 3333: a :32B: of 3334 is one cent
		// over the floored conversion at the :36: rate.
		{"bought amount above the floor", strings.Replace(
			strings.Replace(
				mt304GoodDoc("fwd-mt304-2015", "INST", tradeDate, valueDate),
				":36:1,1", ":36:0,333333333333", 1),
			":32B:EUR110,00", ":32B:EUR33,34", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMT304(tc.doc)
			if err != nil {
				t.Fatalf("ParseMT304: %v", err)
			}
			if _, err := l.ApplyMT304(m, ""); !errors.Is(err, ErrMT304TermsMismatch) {
				t.Errorf("ApplyMT304(%s): err = %v, want ErrMT304TermsMismatch", tc.name, err)
			}
			// A refused instruction books nothing.
			if _, ok := l.Forward(m.Reference); ok {
				t.Errorf("Forward %q exists after refused apply, want none", m.Reference)
			}
		})
	}
}

func TestApplyMT304_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt304ApplyLedger(t, al)
	tradeDate, valueDate := mt304FutureDates()

	// INST: first apply books, replay is a no-op with Duplicate
	// set, and a :20: reused for different terms is refused.
	doc := mt304GoodDoc("fwd-mt304-2016", "INST", tradeDate, valueDate)
	first, err := applyMT304Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT304: %v", err)
	}
	if first.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	second, err := applyMT304Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT304(replay): %v", err)
	}
	if !second.Duplicate {
		t.Error("Duplicate = false on replay, want true")
	}
	if second.Forward == nil || second.Forward.ID != "fwd-mt304-2016" {
		t.Fatalf("replay Forward = %+v, want fwd-mt304-2016", second.Forward)
	}
	if second.Forward.Status != ForwardBooked {
		t.Errorf("replay Forward status = %q, want %q", second.Forward.Status, ForwardBooked)
	}
	conflict := strings.Replace(doc, ":36:1,1", ":36:1,2", 1)
	m, err := ParseMT304(conflict)
	if err != nil {
		t.Fatalf("ParseMT304: %v", err)
	}
	if _, err := l.ApplyMT304(m, ""); !errors.Is(err, ErrMT304TermsMismatch) {
		t.Errorf("ApplyMT304(conflict): err = %v, want ErrMT304TermsMismatch", err)
	}
	// A scope change under the same :20: is a different term too.
	scopeChange := strings.Replace(doc, ":94A:INST", ":94A:ADVI", 1)
	m, err = ParseMT304(scopeChange)
	if err != nil {
		t.Fatalf("ParseMT304: %v", err)
	}
	if _, err := l.ApplyMT304(m, ""); !errors.Is(err, ErrMT304TermsMismatch) {
		t.Errorf("ApplyMT304(scope change): err = %v, want ErrMT304TermsMismatch", err)
	}

	// ADVI: replay is a no-op with Duplicate set, and records
	// nothing twice.
	adviDoc := mt304GoodDoc("fwd-mt304-2017", "ADVI", tradeDate, valueDate)
	if _, err := applyMT304Doc(t, l, adviDoc); err != nil {
		t.Fatalf("ApplyMT304(ADVI): %v", err)
	}
	adviReplay, err := applyMT304Doc(t, l, adviDoc)
	if err != nil {
		t.Fatalf("ApplyMT304(ADVI replay): %v", err)
	}
	if !adviReplay.Duplicate {
		t.Error("ADVI Duplicate = false on replay, want true")
	}
	if adviReplay.Forward != nil {
		t.Errorf("ADVI replay Forward = %+v, want nil", adviReplay.Forward)
	}
	// Exactly one event of each kind across the first applies
	// and the replays: the replays emitted no second event.
	events := readAuditOps(t, al)
	if countOp(events, "mt304_deal_booked") != 1 {
		t.Errorf("mt304_deal_booked count = %d, want 1", countOp(events, "mt304_deal_booked"))
	}
	if countOp(events, "mt304_advice_recorded") != 1 {
		t.Errorf("mt304_advice_recorded count = %d, want 1", countOp(events, "mt304_advice_recorded"))
	}
}

func TestApplyMT304_PastValueDate(t *testing.T) {
	al := mustAuditLog(t)
	l := mt304ApplyLedger(t, al)
	tradeDate, _ := mt304FutureDates()
	pastValue := time.Date(2020, time.January, 2, 0, 0, 0, 0, time.UTC)
	// INST with a past :30V: is refused: BookForward requires
	// SettleAt in the future.
	instDoc := mt304GoodDoc("fwd-mt304-2018", "INST", tradeDate, pastValue)
	m, err := ParseMT304(instDoc)
	if err != nil {
		t.Fatalf("ParseMT304: %v", err)
	}
	if _, err := l.ApplyMT304(m, ""); !errors.Is(err, ErrForwardInvalid) {
		t.Errorf("ApplyMT304(INST past :30V:): err = %v, want ErrForwardInvalid", err)
	}
	if _, ok := l.Forward("fwd-mt304-2018"); ok {
		t.Error("Forward exists after refused INST apply, want none")
	}
	// ADVI with the same past :30V: still records: advice books
	// no contract, so the value date does not gate it. The
	// refused INST left no registry entry, so the :20: is free.
	adviDoc := mt304GoodDoc("fwd-mt304-2018", "ADVI", tradeDate, pastValue)
	if _, err := applyMT304Doc(t, l, adviDoc); err != nil {
		t.Fatalf("ApplyMT304(ADVI past :30V:): %v", err)
	}
	if _, ok := l.MT304Advice("fwd-mt304-2018"); !ok {
		t.Error("MT304Advice after ADVI with past :30V:, want the recorded advice")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt304_advice_recorded") != 1 {
		t.Errorf("mt304_advice_recorded count = %d, want 1", countOp(events, "mt304_advice_recorded"))
	}
	if countOp(events, "mt304_deal_booked") != 0 {
		t.Errorf("mt304_deal_booked count = %d, want 0", countOp(events, "mt304_deal_booked"))
	}
}

func TestApplyMT304_DFormParties(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithFXAccount("fx"), WithAuditLog(al))
	// LT-form account IDs (12 alphanumeric characters) with
	// registered IBANs but no registered BIC: the instruction
	// carries the D form throughout.
	if err := l.SetAccountIBAN("ACCTPAYER001", "DE89370400440532013000", ""); err != nil {
		t.Fatalf("SetAccountIBAN payer: %v", err)
	}
	if err := l.SetAccountIBAN("ACCTPAYEE001", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN payee: %v", err)
	}
	tradeDate, valueDate := mt304FutureDates()
	doc := fmt.Sprintf("{1:F01ACCTPAYER0010000000000}\n"+
		"{2:I304CITIUS33XXXU3003}\n"+
		"{3:{108:%s}}\n"+
		"{4:\n"+
		":15A:\n"+
		":20:%s\n"+
		":22A:NEWT\n"+
		":94A:INST\n"+
		":83D:ACCTPAYER001\n"+
		":82D:ACCTPAYER001\n"+
		":87D:ACCTPAYEE001\n"+
		":15B:\n"+
		":30T:%s\n"+
		":30V:%s\n"+
		":36:1,1\n"+
		":32B:EUR110,00\n"+
		":53D:ACCTPAYEE001\n"+
		":33B:USD100,00\n"+
		":57D:ACCTPAYEE001\n"+
		"-}", "fwd-mt304-2019", "fwd-mt304-2019", mt940ValueDate(tradeDate), mt940ValueDate(valueDate))
	res, err := applyMT304Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT304(D-form): %v", err)
	}
	if res.Forward == nil {
		t.Fatal("Forward = nil, want the booked contract")
	}
	if res.Forward.FromAccount != "ACCTPAYER001" || res.Forward.ToAccount != "ACCTPAYEE001" {
		t.Errorf("Forward accounts = %q/%q, want ACCTPAYER001/ACCTPAYEE001",
			res.Forward.FromAccount, res.Forward.ToAccount)
	}
	if res.Forward.RateNum != 11 || res.Forward.RateDen != 10 || res.Forward.NotionalCents != 10000 {
		t.Errorf("Forward terms = %d/%d notional %d, want 11/10 notional 10000",
			res.Forward.RateNum, res.Forward.RateDen, res.Forward.NotionalCents)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt304_deal_booked") != 1 {
		t.Errorf("mt304_deal_booked count = %d, want 1", countOp(events, "mt304_deal_booked"))
	}
}
