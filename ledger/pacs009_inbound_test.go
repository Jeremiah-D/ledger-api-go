package ledger

import (
	"errors"
	"strings"
	"testing"
)

// pacs009Doc renders one inbound pacs.009.001.06 FI-to-FI credit
// transfer: msgID in GrpHdr, one CdtTrfTxInf moving amount in ccy
// from debtor agent dbtrAgt to creditor agent cdtrAgt, EndToEndId
// e2e (InstrId defaults to e2e).
func pacs009Doc(msgID, e2e, amount, ccy, dbtrAgt, cdtrAgt string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.009.001.06">`)
	sb.WriteString(`<FICdtTrf><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T07:00:00Z</CreDtTm><NbOfTxs>1</NbOfTxs>`)
	sb.WriteString(`<SttlmInf><SttlmMtd>INDA</SttlmMtd></SttlmInf></GrpHdr>`)
	sb.WriteString(`<CdtTrfTxInf><PmtId><InstrId>` + e2e + `</InstrId>`)
	sb.WriteString(`<EndToEndId>` + e2e + `</EndToEndId>`)
	sb.WriteString(`<TxId>` + e2e + `</TxId></PmtId>`)
	sb.WriteString(`<IntrBkSttlmAmt Ccy="` + ccy + `">` + amount + `</IntrBkSttlmAmt>`)
	sb.WriteString(`<IntrBkSttlmDt>2026-11-14</IntrBkSttlmDt>`)
	sb.WriteString(`<InstgAgt><FinInstnId><Othr><Id>` + dbtrAgt + `</Id></Othr></FinInstnId></InstgAgt>`)
	sb.WriteString(`<InstdAgt><FinInstnId><Othr><Id>` + cdtrAgt + `</Id></Othr></FinInstnId></InstdAgt>`)
	sb.WriteString(`<Dbtr><FinInstnId><Othr><Id>` + dbtrAgt + `</Id></Othr></FinInstnId></Dbtr>`)
	sb.WriteString(`<DbtrAgt><FinInstnId><Othr><Id>` + dbtrAgt + `</Id></Othr></FinInstnId></DbtrAgt>`)
	sb.WriteString(`<CdtrAgt><FinInstnId><Othr><Id>` + cdtrAgt + `</Id></Othr></FinInstnId></CdtrAgt>`)
	sb.WriteString(`<Cdtr><FinInstnId><Othr><Id>` + cdtrAgt + `</Id></Othr></FinInstnId></Cdtr>`)
	sb.WriteString(`<ChrgsBr>SLEV</ChrgsBr>`)
	sb.WriteString(`</CdtTrfTxInf></FICdtTrf></Document>`)
	return sb.String()
}

// pacs009InboundLedger creates a ledger with a funded creditor-agent
// account and the pacs.009 settlement account configured, so a
// standalone receipt can book.
func pacs009InboundLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund-cdtr", DebitAccount: "bank-cdtr", CreditAccount: "equity", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	l.SetPacs009SettlementAccount("acct-pacs009-settle")
	return l
}

// Parse reuses the export wire structs in the reverse direction: an
// ExportPacs009 document parses back into the transfer it renders.
func TestParsePacs009RoundTripsExport(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund-alice", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "p9-rt", From: "alice", To: "bob", AmountCents: 2500}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	doc, err := l.ExportPacs009("p9-rt", Pacs009Options{})
	if err != nil {
		t.Fatalf("ExportPacs009: %v", err)
	}
	got, err := ParsePacs009(doc)
	if err != nil {
		t.Fatalf("ParsePacs009(export): %v", err)
	}
	if got.MsgID != "p9-rt/ficdt" {
		t.Errorf("MsgID = %q, want p9-rt/ficdt", got.MsgID)
	}
	if got.EndToEndID != "p9-rt" || got.InstrID != "p9-rt" || got.TxID != "p9-rt" {
		t.Errorf("PmtId = %q/%q/%q, want p9-rt/p9-rt/p9-rt", got.InstrID, got.EndToEndID, got.TxID)
	}
	if got.AmountCents != 2500 || got.Currency != "USD" {
		t.Errorf("amount = %d %s, want 2500 USD", got.AmountCents, got.Currency)
	}
	if got.SettlementMethod != "INDA" || got.ChargesBearer != "SLEV" {
		t.Errorf("routing = %q/%q, want INDA/SLEV", got.SettlementMethod, got.ChargesBearer)
	}
	if got.DebtorAgent != "alice" || got.CreditorAgent != "bob" {
		t.Errorf("agents = %q/%q, want alice/bob", got.DebtorAgent, got.CreditorAgent)
	}
}

// Parse refusals: empty and malformed documents, the wrong schema
// version, a NbOfTxs other than 1, a missing MsgId or CdtrAgt, an
// unreadable or non-positive amount, a bad currency, a malformed
// settlement date, and SttlmMtd/ChrgsBr outside the admitted
// vocabularies (which wrap the export's closed-set sentinels).
func TestParsePacs009Refusals(t *testing.T) {
	valid := pacs009Doc("p9-msg-1", "e2e-p9-1", "25.00", "USD", "bank-dbtr", "bank-cdtr")
	cases := []struct {
		name string
		doc  string
		want error
	}{
		{"empty", "", ErrPacs009Parse},
		{"malformed", "<Document><broken", ErrPacs009Parse},
		{"wrong version", strings.Replace(valid, "pacs.009.001.06", "pacs.009.001.05", 1), ErrPacs009Parse},
		{"two transactions", strings.Replace(valid, "<NbOfTxs>1</NbOfTxs>", "<NbOfTxs>2</NbOfTxs>", 1), ErrPacs009Parse},
		{"missing MsgId", strings.Replace(valid, "<MsgId>p9-msg-1</MsgId>", "", 1), ErrPacs009Parse},
		{"missing CdtrAgt", strings.Replace(valid, `<CdtrAgt><FinInstnId><Othr><Id>bank-cdtr</Id></Othr></FinInstnId></CdtrAgt>`, "", 1), ErrPacs009Parse},
		{"zero amount", strings.Replace(valid, ">25.00<", ">0.00<", 1), ErrPacs009Parse},
		{"non-numeric amount", strings.Replace(valid, ">25.00<", ">25.0x<", 1), ErrPacs009Parse},
		{"bad currency", strings.Replace(valid, `Ccy="USD"`, `Ccy="US"`, 1), ErrPacs009Parse},
		{"bad settlement date", strings.Replace(valid, "<IntrBkSttlmDt>2026-11-14</IntrBkSttlmDt>", "<IntrBkSttlmDt>14/11/2026</IntrBkSttlmDt>", 1), ErrPacs009Parse},
		{"unknown settlement method", strings.Replace(valid, "<SttlmMtd>INDA</SttlmMtd>", "<SttlmMtd>XXXX</SttlmMtd>", 1), ErrPacs009InvalidSettlementMethod},
		{"unknown charges bearer", strings.Replace(valid, "<ChrgsBr>SLEV</ChrgsBr>", "<ChrgsBr>XXXX</ChrgsBr>", 1), ErrPacs009InvalidChargesBearer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePacs009([]byte(tc.doc)); err == nil {
				t.Fatalf("ParsePacs009(%s): err = nil, want an error", tc.name)
			} else if !errors.Is(err, tc.want) {
				t.Errorf("ParsePacs009(%s): err = %v, want it to wrap %v", tc.name, err, tc.want)
			} else if !errors.Is(err, ErrPacs009Parse) {
				t.Errorf("ParsePacs009(%s): err = %v, want it to wrap ErrPacs009Parse too", tc.name, err)
			}
		})
	}
}

// A transfer whose original settled cross-currency is refused with
// the export's FX sentinel, fail-closed, and nothing is parked: a
// single-currency pacs.009 cannot describe it.
func TestApplyPacs009FXTransfer(t *testing.T) {
	l := pacs009InboundLedger(t)
	if _, _, err := l.Post(JournalEntry{ID: "fx-tx", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 2500, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "fx-tx/fx", DebitAccount: "alice", CreditAccount: "fx-clearing", AmountCents: 2500, Currency: "USD"}); err != nil {
		t.Fatalf("fx leg Post: %v", err)
	}
	// The creditor agent must be known, so the FX gate — not the
	// unknown-account gate — is the one under test.
	if _, _, err := l.Post(JournalEntry{ID: "fund-bob", DebitAccount: "bob", CreditAccount: "equity", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("fund bob Post: %v", err)
	}
	tx, err := ParsePacs009([]byte(pacs009Doc("p9-fx-1", "fx-tx", "25.00", "USD", "alice", "bob")))
	if err != nil {
		t.Fatalf("ParsePacs009: %v", err)
	}
	res, err := l.ApplyPacs009(tx, "bank-corr")
	if err == nil {
		t.Fatalf("ApplyPacs009: err = nil, want ErrPacs009FXTransfer (res = %+v)", res)
	}
	if !errors.Is(err, ErrPacs009FXTransfer) {
		t.Errorf("ApplyPacs009: err = %v, want ErrPacs009FXTransfer", err)
	}
	if res.RepairID != "" {
		t.Errorf("RepairID = %q, want empty: an FX original has nothing to repair", res.RepairID)
	}
}

// An EndToEndId replayed under a new MsgId converges on the original
// receipt entry: the entry ID is the idempotency key.
func TestApplyPacs009DuplicateEndToEndID(t *testing.T) {
	l := pacs009InboundLedger(t)
	first, err := ParsePacs009([]byte(pacs009Doc("p9-dup-1", "e2e-dup-1", "12.00", "USD", "bank-dbtr", "bank-cdtr")))
	if err != nil {
		t.Fatalf("ParsePacs009: %v", err)
	}
	res, err := l.ApplyPacs009(first, "bank-corr")
	if err != nil {
		t.Fatalf("ApplyPacs009: %v", err)
	}
	if !res.Applied || res.EntryID != "pacs009/e2e-dup-1" {
		t.Fatalf("first apply: res = %+v, want Applied with entry pacs009/e2e-dup-1", res)
	}
	balBefore := l.BalanceIn("bank-cdtr", "USD")
	second, err := ParsePacs009([]byte(pacs009Doc("p9-dup-2", "e2e-dup-1", "12.00", "USD", "bank-dbtr", "bank-cdtr")))
	if err != nil {
		t.Fatalf("ParsePacs009 replay: %v", err)
	}
	res, err = l.ApplyPacs009(second, "bank-corr")
	if err != nil {
		t.Fatalf("ApplyPacs009 replay: %v", err)
	}
	if !res.Duplicate || res.Applied {
		t.Errorf("replay: res = %+v, want Duplicate true, Applied false", res)
	}
	if res.EntryID != "pacs009/e2e-dup-1" {
		t.Errorf("replay entry = %q, want pacs009/e2e-dup-1", res.EntryID)
	}
	if bal := l.BalanceIn("bank-cdtr", "USD"); bal != balBefore {
		t.Errorf("balance after replay = %d, want %d (unchanged)", bal, balBefore)
	}
}
