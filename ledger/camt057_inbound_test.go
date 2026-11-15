package ledger

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// camt057InboundDoc renders a minimal camt.057 inbound document for
// the given schema version: the counterparty's notification msgID
// (ntfctnID), crediting acctXML (the <Acct> inner XML, IBAN or
// Othr/Id), amount/currency expected on valueDate, from debtorXML
// (the <Dbtr> inner XML, party name or agent BIC), with itemsXML
// (the <Itm> elements). Empty fields omit their elements, so
// callers can build the missing-field shapes.
func camt057InboundDoc(t *testing.T, version, msgID, ntfctnID, acctXML, amount, currency, valueDate, debtorXML, itemsXML string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.057.` + version + `">`)
	sb.WriteString(`<NtfctnToRcv><GrpHdr>`)
	if msgID != "" {
		sb.WriteString(`<MsgId>` + msgID + `</MsgId>`)
	}
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<Ntfctn>`)
	if ntfctnID != "" {
		sb.WriteString(`<Id>` + ntfctnID + `</Id>`)
	}
	if acctXML != "" {
		sb.WriteString(`<Acct><Id>` + acctXML + `</Id></Acct>`)
	}
	sb.WriteString(`<AcctSvcr><FinInstnId><BICFI>CITIUS33</BICFI></FinInstnId></AcctSvcr>`)
	if amount != "" {
		sb.WriteString(`<TtlAmt Ccy="` + currency + `">` + amount + `</TtlAmt>`)
	}
	if valueDate != "" {
		sb.WriteString(`<XpctdValDt>` + valueDate + `</XpctdValDt>`)
	}
	if debtorXML != "" {
		sb.WriteString(`<Dbtr>` + debtorXML + `</Dbtr>`)
	}
	sb.WriteString(itemsXML)
	sb.WriteString(`</Ntfctn></NtfctnToRcv></Document>`)
	return []byte(sb.String())
}

func camt057IBANAcct(iban string) string {
	return `<IBAN>` + iban + `</IBAN>`
}

func camt057OthrAcct(id string) string {
	return `<Othr><Id>` + id + `</Id></Othr>`
}

func camt057PartyDebtor(name string) string {
	return `<Pty><Nm>` + name + `</Nm></Pty>`
}

func camt057AgentDebtor(bic string) string {
	return `<Agt><FinInstnId><BICFI>` + bic + `</BICFI></FinInstnId></Agt>`
}

func camt057Item(id, amount, currency, valueDate string) string {
	return `<Itm><Id>` + id + `</Id><Amt Ccy="` + currency + `">` + amount + `</Amt><XpctdValDt>` + valueDate + `</XpctdValDt></Itm>`
}

// camt057Fixture builds a ledger with an audit log and a registered
// IBAN for bob (GB29NWBK60161331926819), the account the inbound
// notifications credit.
func camt057Fixture(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	return l, al
}

// applyInboundCAMT057 parses and applies one inbound camt.057 for
// the fixture, the inbound-apply test convention.
func applyInboundCAMT057(t *testing.T, l *Ledger, doc []byte, actor string) CAMT057ApplyResult {
	t.Helper()
	n, err := ParseCAMT057(doc)
	if err != nil {
		t.Fatalf("ParseCAMT057: %v", err)
	}
	res, err := l.ApplyCAMT057(n, actor)
	if err != nil {
		t.Fatalf("ApplyCAMT057: %v", err)
	}
	return res
}

func TestParseCAMT057(t *testing.T) {
	doc := camt057InboundDoc(t, "001.07", "msg-1", "ntfctn-1",
		camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), camt057Item("item-1", "250.00", "USD", "2026-11-20"))
	n, err := ParseCAMT057(doc)
	if err != nil {
		t.Fatalf("ParseCAMT057: %v", err)
	}
	if n.MsgID != "msg-1" || n.NotificationID != "ntfctn-1" {
		t.Errorf("ids = %q/%q, want msg-1/ntfctn-1", n.MsgID, n.NotificationID)
	}
	if n.AccountIBAN != "GB29NWBK60161331926819" || n.AccountOthr != "" {
		t.Errorf("acct = IBAN %q Othr %q, want IBAN GB29NWBK60161331926819, no Othr", n.AccountIBAN, n.AccountOthr)
	}
	if n.AmountCents != 25000 || n.Currency != "USD" {
		t.Errorf("amount = %d %s, want 25000 USD", n.AmountCents, n.Currency)
	}
	if n.ExpectedValueDate != "2026-11-20" {
		t.Errorf("value date = %q, want 2026-11-20", n.ExpectedValueDate)
	}
	if n.Debtor != "ACME Corp" {
		t.Errorf("debtor = %q, want ACME Corp", n.Debtor)
	}
	if len(n.Items) != 1 || n.Items[0].ID != "item-1" || n.Items[0].AmountCents != 25000 {
		t.Errorf("items = %+v, want one item-1/25000", n.Items)
	}
}

func TestParseCAMT057OthrAcctAgentDebtor(t *testing.T) {
	doc := camt057InboundDoc(t, "001.07", "msg-2", "ntfctn-2",
		camt057OthrAcct("alice"), "10.50", "EUR", "2026-11-21",
		camt057AgentDebtor("DEUTDEFF"), "")
	n, err := ParseCAMT057(doc)
	if err != nil {
		t.Fatalf("ParseCAMT057: %v", err)
	}
	if n.AccountIBAN != "" || n.AccountOthr != "alice" {
		t.Errorf("acct = IBAN %q Othr %q, want no IBAN, Othr alice", n.AccountIBAN, n.AccountOthr)
	}
	if n.Debtor != "DEUTDEFF" {
		t.Errorf("debtor = %q, want DEUTDEFF", n.Debtor)
	}
	if n.AmountCents != 1050 || n.Currency != "EUR" {
		t.Errorf("amount = %d %s, want 1050 EUR", n.AmountCents, n.Currency)
	}
	if len(n.Items) != 0 {
		t.Errorf("items = %+v, want none", n.Items)
	}
}

func TestParseCAMT057Rejects(t *testing.T) {
	cases := []struct {
		name string
		doc  []byte
	}{
		{"empty document", []byte("  ")},
		{"malformed XML", []byte("<Document><NtfctnToRcv>")},
		{"wrong version", camt057InboundDoc(t, "001.06", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), "")},
		{"missing MsgId", camt057InboundDoc(t, "001.07", "", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), "")},
		{"missing Ntfctn Id", camt057InboundDoc(t, "001.07", "msg-r", "",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), "")},
		{"missing Acct", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			"", "250.00", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), "")},
		{"zero amount", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "0.00", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), "")},
		{"three-decimal amount", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "12.345", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), "")},
		{"bad currency", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "US", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), "")},
		{"missing XpctdValDt", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "",
			camt057PartyDebtor("ACME Corp"), "")},
		{"malformed XpctdValDt", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "20/11/2026",
			camt057PartyDebtor("ACME Corp"), "")},
		{"missing Dbtr", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
			"", "")},
		{"item without Id", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), `<Itm><Amt Ccy="USD">250.00</Amt><XpctdValDt>2026-11-20</XpctdValDt></Itm>`)},
		{"item bad amount", camt057InboundDoc(t, "001.07", "msg-r", "ntfctn-r",
			camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
			camt057PartyDebtor("ACME Corp"), camt057Item("item-1", "abc", "USD", "2026-11-20"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCAMT057(tc.doc); err == nil {
				t.Fatal("ParseCAMT057: got nil error, want a refusal")
			} else if !errors.Is(err, ErrInvalidCAMT057) {
				t.Errorf("error = %v, want ErrInvalidCAMT057", err)
			}
		})
	}
}

func TestApplyCAMT057(t *testing.T) {
	l, al := camt057Fixture(t)
	doc := camt057InboundDoc(t, "001.07", "msg-apply-1", "ntfctn-apply-1",
		camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), camt057Item("item-1", "250.00", "USD", "2026-11-20"))
	versionBefore := l.Version()
	res := applyInboundCAMT057(t, l, doc, "ops")
	if res.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	r := res.Receipt
	if r.MsgID != "msg-apply-1" || r.NotificationID != "ntfctn-apply-1" {
		t.Errorf("receipt ids = %q/%q, want msg-apply-1/ntfctn-apply-1", r.MsgID, r.NotificationID)
	}
	if r.Account != "bob" || r.AmountCents != 25000 || r.Currency != "USD" {
		t.Errorf("receipt = %+v, want account bob 25000 USD", r)
	}
	if r.ExpectedValueDate != "2026-11-20" || r.Debtor != "ACME Corp" || r.Actor != "ops" {
		t.Errorf("receipt = %+v, want 2026-11-20/ACME Corp/ops", r)
	}
	if r.ReceivedAt.IsZero() {
		t.Error("received_at is zero")
	}
	// A forecast only: no journal rows, no version bump.
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (no bump)", l.Version(), versionBefore)
	}
	if len(l.Entries()) != 0 {
		t.Errorf("entries = %d, want 0", len(l.Entries()))
	}
	// The stored receipt is readable back, and the audit event is
	// precise.
	got, ok := l.GetExpectedReceipt("msg-apply-1")
	if !ok || got.AmountCents != 25000 || got.Account != "bob" {
		t.Errorf("GetExpectedReceipt = %+v/%v, want bob/25000", got, ok)
	}
	ev := findOp(readAuditOps(t, al), "camt057_notification_received")
	if ev == nil {
		t.Fatal("no camt057_notification_received audit event")
	}
	if ev.Details["msg_id"] != "msg-apply-1" || ev.Details["acct"] != "bob" {
		t.Errorf("audit details = %+v, want msg_id msg-apply-1 acct bob", ev.Details)
	}
	if ev.Details["amount_cents"] != float64(25000) || ev.Details["currency"] != "USD" || ev.Details["expected_value_date"] != "2026-11-20" {
		t.Errorf("audit details = %+v, want 25000/USD/2026-11-20", ev.Details)
	}
}

func TestApplyCAMT057DuplicateAndConflict(t *testing.T) {
	l, al := camt057Fixture(t)
	doc := camt057InboundDoc(t, "001.07", "msg-dup-1", "ntfctn-dup-1",
		camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), "")
	first := applyInboundCAMT057(t, l, doc, "ops")
	second := applyInboundCAMT057(t, l, doc, "ops")
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if !reflect.DeepEqual(second.Receipt, first.Receipt) {
		t.Errorf("replay receipt = %+v, want %+v", second.Receipt, first.Receipt)
	}
	if countOp(readAuditOps(t, al), "camt057_notification_received") != 1 {
		t.Error("replay emitted a second audit event")
	}
	// The same MsgId for a different notification is a conflict.
	other := camt057InboundDoc(t, "001.07", "msg-dup-1", "ntfctn-dup-1",
		camt057IBANAcct("GB29NWBK60161331926819"), "300.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), "")
	n, err := ParseCAMT057(other)
	if err != nil {
		t.Fatalf("ParseCAMT057: %v", err)
	}
	if _, err := l.ApplyCAMT057(n, "ops"); !errors.Is(err, ErrCAMT057MsgIDConflict) {
		t.Errorf("ApplyCAMT057 conflict = %v, want ErrCAMT057MsgIDConflict", err)
	}
}

func TestApplyCAMT057UnknownAccount(t *testing.T) {
	l, _ := camt057Fixture(t)
	// An IBAN the ledger never registered.
	doc := camt057InboundDoc(t, "001.07", "msg-unk-1", "ntfctn-unk-1",
		camt057IBANAcct("DE89370400440532013000"), "250.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), "")
	n, err := ParseCAMT057(doc)
	if err != nil {
		t.Fatalf("ParseCAMT057: %v", err)
	}
	res, err := l.ApplyCAMT057(n, "ops")
	if !errors.Is(err, ErrCAMT057UnknownAccount) {
		t.Fatalf("ApplyCAMT057 = %v, want ErrCAMT057UnknownAccount", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id is empty on the parked unknown-account path")
	}
	if _, ok := l.GetExpectedReceipt("msg-unk-1"); ok {
		t.Error("unknown-account notification was registered")
	}
	// An Othr/Id the ledger never heard of, either.
	doc2 := camt057InboundDoc(t, "001.07", "msg-unk-2", "ntfctn-unk-2",
		camt057OthrAcct("ghost"), "250.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), "")
	n2, err := ParseCAMT057(doc2)
	if err != nil {
		t.Fatalf("ParseCAMT057: %v", err)
	}
	res2, err := l.ApplyCAMT057(n2, "ops")
	if !errors.Is(err, ErrCAMT057UnknownAccount) || res2.RepairID == "" {
		t.Errorf("ApplyCAMT057 ghost = res %+v err %v, want unknown-account + repair_id", res2, err)
	}
}

func TestApplyCAMT057OthrAccount(t *testing.T) {
	l, _ := camt057Fixture(t)
	// alice has journaled entries but no registered IBAN: the
	// Othr/Id names her directly.
	mustLiquidityPost(t, l, JournalEntry{ID: "e-alice-1", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 1000, Currency: "USD", CreatedAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)})
	doc := camt057InboundDoc(t, "001.07", "msg-othr-1", "ntfctn-othr-1",
		camt057OthrAcct("alice"), "99.99", "USD", "2026-11-20",
		camt057AgentDebtor("DEUTDEFF"), "")
	res := applyInboundCAMT057(t, l, doc, "ops")
	if res.Receipt.Account != "alice" || res.Receipt.AmountCents != 9999 {
		t.Errorf("receipt = %+v, want account alice 9999", res.Receipt)
	}
}

func TestIntradayLiquidityNotifiedInflow(t *testing.T) {
	l, _ := camt057Fixture(t)
	doc := camt057InboundDoc(t, "001.07", "msg-liq-1", "ntfctn-liq-1",
		camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), "")
	applyInboundCAMT057(t, l, doc, "ops")
	// A second notification for another day stays on its own day.
	doc2 := camt057InboundDoc(t, "001.07", "msg-liq-2", "ntfctn-liq-2",
		camt057IBANAcct("GB29NWBK60161331926819"), "100.00", "USD", "2026-11-21",
		camt057PartyDebtor("ACME Corp"), "")
	applyInboundCAMT057(t, l, doc2, "ops")

	day := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	pos := liquidityPos(t, l.IntradayLiquidity(day), "bob", "USD")
	if pos.NotifiedInflowCents != 25000 {
		t.Errorf("notified inflow = %d, want 25000", pos.NotifiedInflowCents)
	}
	if pos.NetPositionCents != 25000 {
		t.Errorf("net = %d, want 25000", pos.NetPositionCents)
	}
	// The other day's notification does not leak in.
	otherDay := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	other := liquidityPos(t, l.IntradayLiquidity(otherDay), "bob", "USD")
	if other.NotifiedInflowCents != 10000 {
		t.Errorf("other-day notified inflow = %d, want 10000", other.NotifiedInflowCents)
	}
}

func TestParkCAMT057Unparsed(t *testing.T) {
	l, _ := camt057Fixture(t)
	// A wrong-version document that still names its MsgId parks.
	doc := camt057InboundDoc(t, "001.06", "msg-park-1", "ntfctn-park-1",
		camt057IBANAcct("GB29NWBK60161331926819"), "250.00", "USD", "2026-11-20",
		camt057PartyDebtor("ACME Corp"), "")
	if _, err := ParseCAMT057(doc); err == nil {
		t.Fatal("ParseCAMT057 accepted a 001.06 document")
	}
	id, err := l.ParkCAMT057Unparsed(doc, "ops")
	if err != nil {
		t.Fatalf("ParkCAMT057Unparsed: %v", err)
	}
	if id == "" {
		t.Error("repair_id is empty for a named unparsed document")
	}
	// Garbage names nothing: nothing to park.
	id, err = l.ParkCAMT057Unparsed([]byte("not xml at all"), "ops")
	if err != nil {
		t.Fatalf("ParkCAMT057Unparsed garbage: %v", err)
	}
	if id != "" {
		t.Errorf("repair_id = %q for garbage, want empty", id)
	}
}
