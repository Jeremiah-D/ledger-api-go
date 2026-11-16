package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt300ApplyLedger books the mt300TestLedger forward on a ledger
// with an audit log, the fixture for the MT300 apply tests.
func mt300ApplyLedger(t *testing.T, id string, al *AuditLog) (*Ledger, *FXForward) {
	t.Helper()
	l := New(WithFXAccount("fx"), WithAuditLog(al))
	if err := l.SetAccountIBAN("payer", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN payer: %v", err)
	}
	if err := l.SetAccountIBAN("payee", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN payee: %v", err)
	}
	now := time.Now().UTC()
	c := mustBookForwardLocked(t, l, forwardBooking(id, now.Add(48*time.Hour)), now)
	return l, c
}

// applyMT300Doc parses doc and applies it with no explicit actor,
// so the audit event carries the ApplyMT300 default attribution
// the spec names, failing the test on parse errors: the gates
// under test are the apply gates, not the parse gates.
func applyMT300Doc(t *testing.T, l *Ledger, doc string) (MT300ApplyResult, error) {
	t.Helper()
	m, err := ParseMT300(doc)
	if err != nil {
		t.Fatalf("ParseMT300: %v", err)
	}
	return l.ApplyMT300(m, "")
}

func TestParseMT300_RoundTrip(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	m, err := ParseMT300(doc)
	if err != nil {
		t.Fatalf("ParseMT300(export): %v", err)
	}
	if m.SenderLT != "DEUTDEFFXXX" || m.ReceiverLT != "NWBKGB2LXXX" {
		t.Errorf("LTs = %q/%q, want DEUTDEFFXXX/NWBKGB2LXXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != c.ID {
		t.Errorf(":20: = %q, want %q", m.Reference, c.ID)
	}
	if m.Variant != "NEWT" {
		t.Errorf(":22A: = %q, want NEWT", m.Variant)
	}
	if m.CommonReference != "DEUTNW1001" {
		t.Errorf(":22C: = %q, want DEUTNW1001", m.CommonReference)
	}
	if m.SenderParty != "DEUTDEFF" || !m.SenderPartyBIC {
		t.Errorf(":82a: = %q (BIC=%v), want DEUTDEFF (BIC)", m.SenderParty, m.SenderPartyBIC)
	}
	if m.ReceiverParty != "NWBKGB2L" || !m.ReceiverPartyBIC {
		t.Errorf(":87a: = %q (BIC=%v), want NWBKGB2L (BIC)", m.ReceiverParty, m.ReceiverPartyBIC)
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
	if m.SoldCurrency != "USD" || m.SoldCents != 10000 {
		t.Errorf(":33B: = %s %d, want USD 10000", m.SoldCurrency, m.SoldCents)
	}
	if m.AccountParty != "NWBKGB2L" || !m.AccountPartyBIC {
		t.Errorf(":57a: = %q (BIC=%v), want NWBKGB2L (BIC)", m.AccountParty, m.AccountPartyBIC)
	}
}

func TestParseMT300_RejectsBadBlockStructure(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	good, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT202", strings.Replace(good, "{2:I300", "{2:I202", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:fwd-mt300-1001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":30T:"+mt940ValueDate(c.BookedAt)+"\n:30V:"+mt940ValueDate(c.SettleAt)+"\n",
			":30V:"+mt940ValueDate(c.SettleAt)+"\n:30T:"+mt940ValueDate(c.BookedAt)+"\n", 1)},
		{"missing :15B:", strings.Replace(good, ":15B:\n", "", 1)},
		{"missing :36:", strings.Replace(good, ":36:1,1\n", "", 1)},
		{"bad :30V: date", strings.Replace(good, ":30V:"+mt940ValueDate(c.SettleAt), ":30V:991332", 1)},
		{"bad :36: rate", strings.Replace(good, ":36:1,1", ":36:1.1", 1)},
		{"bad :32B: amount", strings.Replace(good, ":32B:EUR110,00", ":32B:EUR110.00", 1)},
		{"bad :22C: shape", strings.Replace(good, ":22C:DEUTNW1001", ":22C:DEUTNW100X", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT300(tc.doc); err == nil {
				t.Errorf("ParseMT300(%s): want error, got nil", tc.name)
			}
		})
	}
}

func TestParseMT300_RejectsUnknownField(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	good, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}

	cases := []struct {
		name string
		doc  string
	}{
		// An extra field between :33B: and :57A: — the line
		// count grows past 18, so the framing fails first.
		{"extra :72: field", strings.Replace(good, ":57A:NWBKGB2L", ":72:EXTRA\n:57A:NWBKGB2L", 1)},
		// A field tag the core set does not know, in place of
		// :57A:.
		{":72: instead of :57A:", strings.Replace(good, ":57A:NWBKGB2L", ":72:EXTRA", 1)},
		// A D-variant party the export never renders for a BIC
		// party is still a known option — but an unknown option
		// letter is not.
		{":82C: option", strings.Replace(good, ":82A:DEUTDEFF", ":82C:DEUTDEFF", 1)},
		// A field smuggled outside block 4, after the trailer.
		{"field after trailer", good + "\n:72:EXTRA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT300(tc.doc); err == nil {
				t.Errorf("ParseMT300(%s): want error, got nil", tc.name)
			}
		})
	}
}

func TestParseMT300_AcceptsCRLF(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	m, err := ParseMT300(crlf)
	if err != nil {
		t.Fatalf("ParseMT300(CRLF): %v", err)
	}
	if m.CommonReference != "DEUTNW1001" {
		t.Errorf(":22C: = %q, want DEUTNW1001", m.CommonReference)
	}
	// A single trailing newline after the `-}` trailer is
	// tolerated, like ParseMT202.
	m, err = ParseMT300(doc + "\n")
	if err != nil {
		t.Fatalf("ParseMT300(trailing newline): %v", err)
	}
	if m.Reference != c.ID {
		t.Errorf(":20: = %q, want %q", m.Reference, c.ID)
	}
}

func TestParseMT300_RejectsNonNewtVariant(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	for _, variant := range []string{"AMND", "CANC"} {
		t.Run(variant, func(t *testing.T) {
			bad := strings.Replace(doc, ":22A:NEWT", ":22A:"+variant, 1)
			if _, err := ParseMT300(bad); err == nil {
				t.Errorf("ParseMT300(:22A:%s): want error, got nil", variant)
			}
		})
	}
}

func TestApplyMT300_ConfirmsMatchedForward(t *testing.T) {
	al := mustAuditLog(t)
	l, c := mt300ApplyLedger(t, "fwd-mt300-1001", al)
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	if ref, ok := l.ForwardConfirmed(c.ID); ok || ref != "" {
		t.Fatalf("ForwardConfirmed before apply: ref=%q ok=%v, want unconfirmed", ref, ok)
	}
	res, err := applyMT300Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT300: %v", err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	if res.Forward == nil || res.Forward.ID != c.ID {
		t.Fatalf("Forward = %+v, want %q", res.Forward, c.ID)
	}
	if res.Forward.Status != ForwardBooked {
		t.Errorf("Forward status = %q, want %q (confirmation moves no lifecycle state)", res.Forward.Status, ForwardBooked)
	}
	if res.CommonReference != "DEUTNW1001" {
		t.Errorf("CommonReference = %q, want DEUTNW1001", res.CommonReference)
	}
	// The confirmation marker: the forward reads as confirmed,
	// under the :20: that confirmed it.
	if ref, ok := l.ForwardConfirmed(c.ID); !ok || ref != c.ID {
		t.Errorf("ForwardConfirmed = %q/%v, want %q/true", ref, ok, c.ID)
	}
	// The audit trail carries exactly one match event, with the
	// specified details and actor.
	events := readAuditOps(t, al)
	matched := findOp(events, "mt300_confirmation_matched")
	if matched == nil {
		t.Fatalf("no mt300_confirmation_matched event in %d events", len(events))
	}
	if matched.Actor != "ApplyMT300" {
		t.Errorf("audit actor = %q, want ApplyMT300", matched.Actor)
	}
	if matched.Details["msg_id"] != c.ID {
		t.Errorf("audit msg_id = %v, want %q", matched.Details["msg_id"], c.ID)
	}
	if matched.Details["common_ref"] != "DEUTNW1001" {
		t.Errorf("audit common_ref = %v, want DEUTNW1001", matched.Details["common_ref"])
	}
	if matched.Details["forward_id"] != c.ID {
		t.Errorf("audit forward_id = %v, want %q", matched.Details["forward_id"], c.ID)
	}
	if countOp(events, "mt300_confirmation_matched") != 1 {
		t.Errorf("mt300_confirmation_matched count = %d, want 1", countOp(events, "mt300_confirmation_matched"))
	}
}

func TestApplyMT300_RefusesUnknownForward(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	// The :22C: names a forward that does not exist: the last 4
	// digits point at another contract ID. The :20: stays the
	// booked forward's ID — only the addressing key is wrong.
	unknown := strings.Replace(doc, ":22C:DEUTNW1001", ":22C:DEUTNW9999", 1)
	m, err := ParseMT300(unknown)
	if err != nil {
		t.Fatalf("ParseMT300: %v", err)
	}
	if _, err := l.ApplyMT300(m, "counterparty-bank"); !errors.Is(err, ErrForwardNotFound) {
		t.Errorf("ApplyMT300(unknown forward): err = %v, want ErrForwardNotFound", err)
	}
	// Nothing was recorded: the booked forward stays
	// unconfirmed.
	if _, ok := l.ForwardConfirmed(c.ID); ok {
		t.Error("ForwardConfirmed after refused apply, want unconfirmed")
	}
}

func TestApplyMT300_RefusesRateMismatch(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	// The counterparty confirms at 1,2 while the contract locks
	// 1,1: the whole confirmation is refused.
	bad := strings.Replace(doc, ":36:1,1", ":36:1,2", 1)
	m, err := ParseMT300(bad)
	if err != nil {
		t.Fatalf("ParseMT300: %v", err)
	}
	if _, err := l.ApplyMT300(m, "counterparty-bank"); !errors.Is(err, ErrMT300TermsMismatch) {
		t.Errorf("ApplyMT300(rate mismatch): err = %v, want ErrMT300TermsMismatch", err)
	}
	if _, ok := l.ForwardConfirmed(c.ID); ok {
		t.Error("ForwardConfirmed after refused apply, want unconfirmed")
	}
}

func TestApplyMT300_RefusesAmountMismatch(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	cases := []struct {
		name string
		doc  string
	}{
		{"bought amount off by one cent", strings.Replace(doc, ":32B:EUR110,00", ":32B:EUR110,01", 1)},
		{"bought currency wrong", strings.Replace(doc, ":32B:EUR110,00", ":32B:USD110,00", 1)},
		{"sold amount off by one cent", strings.Replace(doc, ":33B:USD100,00", ":33B:USD100,01", 1)},
		{"sold currency wrong", strings.Replace(doc, ":33B:USD100,00", ":33B:EUR100,00", 1)},
		{"value date wrong", strings.Replace(doc,
			":30V:"+mt940ValueDate(c.SettleAt),
			":30V:"+mt940ValueDate(c.SettleAt.AddDate(0, 0, 1)), 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMT300(tc.doc)
			if err != nil {
				t.Fatalf("ParseMT300: %v", err)
			}
			if _, err := l.ApplyMT300(m, "counterparty-bank"); !errors.Is(err, ErrMT300TermsMismatch) {
				t.Errorf("ApplyMT300(%s): err = %v, want ErrMT300TermsMismatch", tc.name, err)
			}
		})
	}
	if _, ok := l.ForwardConfirmed(c.ID); ok {
		t.Error("ForwardConfirmed after refused applies, want unconfirmed")
	}
}

func TestApplyMT300_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l, c := mt300ApplyLedger(t, "fwd-mt300-1001", al)
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	first, err := applyMT300Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT300: %v", err)
	}
	if first.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	// Replay the same :20: with the same terms: a no-op
	// returning the current forward, Duplicate set.
	second, err := applyMT300Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT300(replay): %v", err)
	}
	if !second.Duplicate {
		t.Error("Duplicate = false on replay, want true")
	}
	if second.Forward == nil || second.Forward.ID != c.ID {
		t.Fatalf("replay Forward = %+v, want %q", second.Forward, c.ID)
	}
	if second.Forward.Status != ForwardBooked {
		t.Errorf("replay Forward status = %q, want %q", second.Forward.Status, ForwardBooked)
	}
	// The same :20: with different terms is a conflict, never a
	// silent overwrite: the rate differs, the forward stays as
	// first confirmed.
	conflict := strings.Replace(doc, ":36:1,1", ":36:1,2", 1)
	m, err := ParseMT300(conflict)
	if err != nil {
		t.Fatalf("ParseMT300: %v", err)
	}
	if _, err := l.ApplyMT300(m, "counterparty-bank"); !errors.Is(err, ErrMT300MsgIDConflict) {
		t.Errorf("ApplyMT300(conflict): err = %v, want ErrMT300MsgIDConflict", err)
	}
	// Exactly one match event across the first apply and the
	// replay: the replay emitted no second event.
	events := readAuditOps(t, al)
	if countOp(events, "mt300_confirmation_matched") != 1 {
		t.Errorf("mt300_confirmation_matched count = %d, want 1", countOp(events, "mt300_confirmation_matched"))
	}
}
