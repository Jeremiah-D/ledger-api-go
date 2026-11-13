package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pain008StatusFixture builds a ledger with an audit log, IBANs, a
// live per-month mandate (m-008), and two booked debits (d-1 in the
// first month, d-2 in the second) — ready for pain.008 export.
func pain008StatusFixture(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	seedBalance(t, l, "cust", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	day := pain008Day()
	monthStart := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-008",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerMonth,
		ValidFrom:      monthStart,
		ValidUntil:     monthStart.AddDate(0, 3, 0),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	pain008Debit(t, l, "d-1", monthStart.AddDate(0, 0, 9))
	pain008Debit(t, l, "d-2", monthStart.AddDate(0, 1, 9))
	return l, al
}

// mustExportPain008MsgID exports and returns the deterministic MsgId,
// extracted from the rendered document's GrpHdr.
func mustExportPain008MsgID(t *testing.T, l *Ledger, opts Pain008ExportOptions) string {
	t.Helper()
	doc, err := l.ExportPain008("m-008", opts)
	if err != nil {
		t.Fatalf("ExportPain008: %v", err)
	}
	s := string(doc)
	const open, close = "<MsgId>", "</MsgId>"
	i := strings.Index(s, open)
	j := strings.Index(s, close)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("exported document has no MsgId:\n%s", s)
	}
	return s[i+len(open) : j]
}

// pain008TxState finds an entry's state in a Pain008Instruction.
func pain008TxState(t *testing.T, rec Pain008Instruction, entryID string) Pain008TxState {
	t.Helper()
	for _, tx := range rec.Txs {
		if tx.EntryID == entryID {
			return tx
		}
	}
	t.Fatalf("entry %q not in instruction", entryID)
	return Pain008TxState{}
}

// pain008StatusEvents reads the audit log and returns the
// pain008_status events in order.
func pain008StatusEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "pain008_status" {
			out = append(out, ev)
		}
	}
	return out
}

// Exporting registers the instruction: born "registered", immediately
// moved to "exported", one audit event, every entry exported. A
// duplicate export is idempotent: same record, no new audit event.
func TestPain008InstructionRegistration(t *testing.T) {
	l, al := pain008StatusFixture(t)

	msgID := mustExportPain008MsgID(t, l, pain008Opts())
	rec, ok := l.Pain008Instruction(msgID)
	if !ok {
		t.Fatalf("Pain008Instruction(%q): not registered", msgID)
	}
	if rec.State != Pain008StateExported {
		t.Errorf("state = %q, want %q", rec.State, Pain008StateExported)
	}
	if rec.MandateID != "m-008" {
		t.Errorf("mandate_id = %q, want %q", rec.MandateID, "m-008")
	}
	if rec.TxCount != 2 || rec.CtrlSum != 2000 || rec.Currency != "USD" {
		t.Errorf("tx_count/ctrl_sum/currency = %d/%d/%q, want 2/2000/USD", rec.TxCount, rec.CtrlSum, rec.Currency)
	}
	for _, entryID := range []string{"d-1", "d-2"} {
		tx := pain008TxState(t, rec, entryID)
		if tx.State != Pain008StateExported {
			t.Errorf("%s state = %q, want %q", entryID, tx.State, Pain008StateExported)
		}
		if tx.AmountCents != 1000 || tx.Currency != "USD" {
			t.Errorf("%s amount/currency = %d/%q, want 1000/USD", entryID, tx.AmountCents, tx.Currency)
		}
	}
	if _, err := l.ExportPain008("m-008", pain008Opts()); err != nil {
		t.Fatalf("duplicate ExportPain008: %v", err)
	}
	// Only the export's own registered -> exported event exists.
	if events := pain008StatusEvents(t, al); len(events) != 1 {
		t.Errorf("pain008_status events = %d, want 1 (export only)", len(events))
	}
}

// ACSC advances to accepted, RJCT advances to rejected with the bank's
// reason code verbatim in the audit event; a mixed instruction stays
// exported.
func TestPain008ApplyACSCAndRJCT(t *testing.T) {
	l, al := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02",
		pain002TxFrag("d-1", "ACSC", "", ""),
		pain002TxFrag("d-2", "RJCT", "AC01", "Account identifier invalid"),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if report.OrgnlMsgID != msgID {
		t.Errorf("OrgnlMsgID = %q, want %q", report.OrgnlMsgID, msgID)
	}

	rec, err := l.ApplyPain002ToPain008(report, "bank-status")
	if err != nil {
		t.Fatalf("ApplyPain002ToPain008: %v", err)
	}
	if got := pain008TxState(t, rec, "d-1").State; got != Pain008StateAccepted {
		t.Errorf("d-1 state = %q, want %q", got, Pain008StateAccepted)
	}
	d2 := pain008TxState(t, rec, "d-2")
	if d2.State != Pain008StateRejected {
		t.Errorf("d-2 state = %q, want %q", d2.State, Pain008StateRejected)
	}
	if d2.Reason != "AC01: Account identifier invalid" {
		t.Errorf("d-2 reason = %q, want %q", d2.Reason, "AC01: Account identifier invalid")
	}
	// Mixed accepted/rejected: the instruction stays exported.
	if rec.State != Pain008StateExported {
		t.Errorf("instruction state = %q, want %q", rec.State, Pain008StateExported)
	}

	// The RJCT reason code lands verbatim in the hash-chained audit
	// event, with the entry id and the verdict in the details.
	events := pain008StatusEvents(t, al)
	found := false
	for _, ev := range events {
		if ev.Details["entry_id"] == "d-2" && ev.Details["to"] == Pain008StateRejected {
			found = true
			if ev.Details["verdict"] != Pain008StateRejected {
				t.Errorf("audit verdict = %q, want %q", ev.Details["verdict"], Pain008StateRejected)
			}
			if ev.Details["reason_code"] != "AC01" {
				t.Errorf("audit reason_code = %q, want AC01", ev.Details["reason_code"])
			}
			if ev.Details["reason"] != "AC01: Account identifier invalid" {
				t.Errorf("audit reason = %q, want verbatim bank reason", ev.Details["reason"])
			}
		}
	}
	if !found {
		t.Error("no pain008_status audit event for the d-2 rejection")
	}
}

// ACCP (accepted, settlement pending) maps to the ledger's single
// accepted state; completion is confirmed by the operator, not
// inferred from the bank's wording.
func TestPain008ApplyACCPMapsToAccepted(t *testing.T) {
	l, _ := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02", pain002TxFrag("d-1", "ACCP", "", ""))
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	rec, err := l.ApplyPain002ToPain008(report, "bank-status")
	if err != nil {
		t.Fatalf("ApplyPain002ToPain008: %v", err)
	}
	if got := pain008TxState(t, rec, "d-1").State; got != Pain008StateAccepted {
		t.Errorf("d-1 state = %q, want %q", got, Pain008StateAccepted)
	}
	// The unmentioned d-2 stays exported.
	if got := pain008TxState(t, rec, "d-2").State; got != Pain008StateExported {
		t.Errorf("d-2 state = %q, want %q", got, Pain008StateExported)
	}
}

// Every entry accepted: the instruction itself reaches accepted.
func TestPain008ApplyAllAccepted(t *testing.T) {
	l, _ := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02",
		pain002TxFrag("d-1", "ACSC", "", ""),
		pain002TxFrag("d-2", "ACCP", "", ""),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	rec, err := l.ApplyPain002ToPain008(report, "bank-status")
	if err != nil {
		t.Fatalf("ApplyPain002ToPain008: %v", err)
	}
	if rec.State != Pain008StateAccepted {
		t.Errorf("instruction state = %q, want %q", rec.State, Pain008StateAccepted)
	}
}

// PDNG is not a verdict: the entry stays exported and nothing is
// recorded.
func TestPain008PDNGNoOp(t *testing.T) {
	l, al := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02",
		pain002TxFrag("d-1", "PDNG", "", ""),
		pain002TxFrag("d-2", "PDNG", "", ""),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	rec, err := l.ApplyPain002ToPain008(report, "bank-status")
	if err != nil {
		t.Fatalf("ApplyPain002ToPain008: %v", err)
	}
	if rec.State != Pain008StateExported {
		t.Errorf("instruction state = %q, want %q", rec.State, Pain008StateExported)
	}
	for _, entryID := range []string{"d-1", "d-2"} {
		if got := pain008TxState(t, rec, entryID).State; got != Pain008StateExported {
			t.Errorf("%s state = %q, want %q", entryID, got, Pain008StateExported)
		}
	}
	// Only the export's own registered -> exported event exists.
	if events := pain008StatusEvents(t, al); len(events) != 1 {
		t.Errorf("pain008_status events = %d, want 1 (export only)", len(events))
	}
}

// Re-applying the same report is idempotent: same-state verdict replay
// records nothing new.
func TestPain008ReplayIdempotent(t *testing.T) {
	l, al := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02",
		pain002TxFrag("d-1", "ACSC", "", ""),
		pain002TxFrag("d-2", "RJCT", "AM09", "Wrong amount"),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002ToPain008(report, "bank-status"); err != nil {
		t.Fatalf("first ApplyPain002ToPain008: %v", err)
	}
	rec, err := l.ApplyPain002ToPain008(report, "bank-status")
	if err != nil {
		t.Fatalf("second ApplyPain002ToPain008: %v", err)
	}
	if got := pain008TxState(t, rec, "d-1").State; got != Pain008StateAccepted {
		t.Errorf("d-1 state = %q, want %q", got, Pain008StateAccepted)
	}
	// Export (1) + first apply (2 transitions) = 3 events; the replay
	// adds none.
	if events := pain008StatusEvents(t, al); len(events) != 3 {
		t.Errorf("pain008_status events = %d, want 3 (replay must add none)", len(events))
	}
}

// A report for an instruction the ledger never exported is a client
// error, not a silent drop.
func TestPain008ApplyUnknownInstruction(t *testing.T) {
	l, _ := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, "pain008-0-0-never-exported", "pain.008.001.02", pain002TxFrag("d-1", "ACSC", "", ""))
	_ = msgID // the real instruction exists; the report names another
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002ToPain008(report, "bank-status"); !errors.Is(err, ErrUnknownPain008Instruction) {
		t.Errorf("ApplyPain002ToPain008 err = %v, want ErrUnknownPain008Instruction", err)
	}
}

// A report naming an entry outside the instruction refuses the whole
// batch: the verdict cannot be attributed.
func TestPain008ApplyUnknownEntry(t *testing.T) {
	l, _ := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02", pain002TxFrag("d-nope", "ACSC", "", ""))
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002ToPain008(report, "bank-status"); !errors.Is(err, ErrUnknownPain008Tx) {
		t.Errorf("ApplyPain002ToPain008 err = %v, want ErrUnknownPain008Tx", err)
	}
	// Nothing applied: the instruction is untouched.
	rec, ok := l.Pain008Instruction(msgID)
	if !ok {
		t.Fatalf("Pain008Instruction(%q): lost after refused batch", msgID)
	}
	if got := pain008TxState(t, rec, "d-1").State; got != Pain008StateExported {
		t.Errorf("d-1 state = %q, want %q (refused batch must apply nothing)", got, Pain008StateExported)
	}
}

// Rejected is terminal: a later ACSC for the same entry is an illegal
// transition, not a resurrection.
func TestPain008IllegalTransition(t *testing.T) {
	l, _ := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02", pain002TxFrag("d-1", "RJCT", "AC01", "Account identifier invalid"))
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002ToPain008(report, "bank-status"); err != nil {
		t.Fatalf("ApplyPain002ToPain008 RJCT: %v", err)
	}
	doc = pain002TestDocNm(t, msgID, "pain.008.001.02", pain002TxFrag("d-1", "ACSC", "", ""))
	report, err = ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002ToPain008(report, "bank-status"); !errors.Is(err, ErrIllegalPain008Transition) {
		t.Errorf("ApplyPain002ToPain008 err = %v, want ErrIllegalPain008Transition", err)
	}
	rec, _ := l.Pain008Instruction(msgID)
	if got := pain008TxState(t, rec, "d-1").State; got != Pain008StateRejected {
		t.Errorf("d-1 state = %q, want %q (illegal transition must leave it rejected)", got, Pain008StateRejected)
	}
}

// The parser now accepts the second original message name: a report
// answering pain.008.001.02 with a pain008- MsgId parses, while the
// old refusals still hold — pacs.010 stays out under either name, and
// the name/MsgId pairing is enforced both ways.
func TestParsePain002Pain008Family(t *testing.T) {
	l, _ := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())
	tx := pain002TxFrag("d-1", "ACSC", "", "")

	// Genuine pain.008 answer parses.
	if _, err := ParsePain002(pain002TestDocNm(t, msgID, "pain.008.001.02", tx)); err != nil {
		t.Errorf("pain.002 for pain.008 = %v, want nil", err)
	}
	// pacs.010 stays refused under the pain.008 name too.
	doc := pain002TestDocNm(t, "pacs010-0-0-m10-d1", "pain.008.001.02", tx)
	if _, err := ParsePain002(doc); !errors.Is(err, ErrPain002ForeignMsgID) {
		t.Errorf("pain.002 naming pacs010 MsgId under pain.008 name = %v, want ErrPain002ForeignMsgID", err)
	}
	// A pain.008 MsgId under the pain.001 name is still refused.
	doc = pain002TestDocNm(t, msgID, "pain.001.001.03", tx)
	if _, err := ParsePain002(doc); !errors.Is(err, ErrPain002ForeignMsgID) {
		t.Errorf("pain.002 naming pain008 MsgId under pain.001 name = %v, want ErrPain002ForeignMsgID", err)
	}
	// A non-pain.008 MsgId under the pain.008 name is refused.
	doc = pain002TestDocNm(t, "pain001-0-0-x", "pain.008.001.02", tx)
	if _, err := ParsePain002(doc); !errors.Is(err, ErrPain002ForeignMsgID) {
		t.Errorf("pain.002 naming pain001 MsgId under pain.008 name = %v, want ErrPain002ForeignMsgID", err)
	}
}
