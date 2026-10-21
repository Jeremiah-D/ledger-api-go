package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// achReturnReceiver is the test receiver for return-file entries.
func achReturnReceiver() NACHAReceiver {
	return NACHAReceiver{
		Routing:        "021000089",
		AccountNumber:  "987654321",
		IndividualName: "ACME SUPPLIER",
	}
}

// buildAddenda98 renders an Addenda Type 98 record for the given entry
// detail line, stamping the entry's own trace number as the original
// entry trace (positions 7-21), like a bank answering our file.
func buildAddenda98(t *testing.T, entryLine, reasonCode string) string {
	t.Helper()
	if len(entryLine) != nachaRecordLen || !strings.HasPrefix(entryLine, "6") {
		t.Fatalf("buildAddenda98: not an entry detail line: %q", entryLine)
	}
	pad := func(s string, w int) string {
		if len(s) > w {
			t.Fatalf("buildAddenda98: field %q exceeds %d", s, w)
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	var b strings.Builder
	b.WriteString("7")                  // record type
	b.WriteString("98")                 // addenda type code
	b.WriteString(pad(reasonCode, 3))   // positions 4-6: return reason code (padded, still invalid if bad)
	b.WriteString(" ")                  // position 7: reserved blank
	b.WriteString(entryLine[79:94])       // positions 8-22: original entry trace
	b.WriteString(strings.Repeat(" ", 6)) // positions 23-28: date of death (blank)
	b.WriteString(pad(entryLine[3:12], 15)) // positions 29-43: original receiving DFI
	b.WriteString(pad("RETURNED "+reasonCode, 37)) // positions 44-80: addenda info
	b.WriteString(entryLine[79:83])       // positions 81-84: addenda trace sequence
	b.WriteString(strings.Repeat(" ", 10)) // positions 85-94: reserved blank
	rec := b.String()
	if len(rec) != nachaRecordLen {
		t.Fatalf("buildAddenda98: record length %d, want 94", len(rec))
	}
	return rec
}

// buildNACHAReturnFile renders a bank return file for the given
// (payout, reasonCode) pairs: it generates the origination entries
// with the real builder (so the entry detail layout is exactly what
// the bank would answer), splices one Addenda 98 after each entry
// detail, and joins with "\n".
func buildNACHAReturnFile(t *testing.T, payouts []PayoutInstruction, reasonCodes []string) string {
	t.Helper()
	if len(payouts) != len(reasonCodes) {
		t.Fatal("buildNACHAReturnFile: payouts/reasonCodes length mismatch")
	}
	entries := make([]NACHAEntry, len(payouts))
	for i, p := range payouts {
		e, err := PayoutToNACHAEntry(p, achReturnReceiver())
		if err != nil {
			t.Fatalf("PayoutToNACHAEntry: %v", err)
		}
		entries[i] = e
	}
	file, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{{
		Config: NACHABatchConfig{
			ServiceClassCode:        "220",
			CompanyName:             "ACME CORP",
			CompanyID:               "1122334455",
			EntryClassCode:          "CCD",
			CompanyEntryDescription: "VENDOR PAY",
			EffectiveEntryDate:      time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
			OriginatingDFI:          "12104288",
			BatchNumber:             1,
		},
		Entries: entries,
	}})
	if err != nil {
		t.Fatalf("GenerateNACHAFile: %v", err)
	}
	var out []string
	entryIdx := 0
	for _, ln := range strings.Split(file, "\n") {
		out = append(out, ln)
		if strings.HasPrefix(ln, "6") {
			out = append(out, buildAddenda98(t, ln, reasonCodes[entryIdx]))
			entryIdx++
		}
	}
	if entryIdx != len(payouts) {
		t.Fatalf("buildNACHAReturnFile: spliced %d addenda, want %d", entryIdx, len(payouts))
	}
	return strings.Join(out, "\n")
}

// seedSubmittedPayout drafts, approves, and submits one payout,
// returning it with the clearing entry journaled.
func seedSubmittedPayout(t *testing.T, l *Ledger, id string, amount int64) PayoutInstruction {
	t.Helper()
	draftPayout(t, l, id, amount)
	return approveSubmitPayout(t, l, id, "bob")
}

func TestParseNACHAReturn_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-1", 250_000)
	file := buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R01"})
	returns, err := ParseNACHAReturn(file)
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	if len(returns) != 1 {
		t.Fatalf("returns = %d, want 1", len(returns))
	}
	r := returns[0]
	if r.ReasonCode != "R01" {
		t.Errorf("ReasonCode = %q, want R01", r.ReasonCode)
	}
	if r.IndividualID != "po-ret-1" {
		t.Errorf("IndividualID = %q, want po-ret-1", r.IndividualID)
	}
	if r.AmountCents != 250_000 {
		t.Errorf("AmountCents = %d, want 250000", r.AmountCents)
	}
	if r.TraceNumber == "" || len(r.TraceNumber) != 15 {
		t.Errorf("TraceNumber = %q, want the 15-char original trace", r.TraceNumber)
	}
	if r.TransactionCode != "22" {
		t.Errorf("TransactionCode = %q, want 22", r.TransactionCode)
	}
	if r.Routing != "021000089" {
		t.Errorf("Routing = %q, want 021000089", r.Routing)
	}
	if r.AccountNumber != "987654321" {
		t.Errorf("AccountNumber = %q, want 987654321", r.AccountNumber)
	}
	if !strings.Contains(r.AddendaInfo, "R01") {
		t.Errorf("AddendaInfo = %q, want it to mention the reason code", r.AddendaInfo)
	}
}

func TestParseNACHAReturn_InvalidReasonCode(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-2", 100_000)
	for _, bad := range []string{"R99", "X01", "R00", "R86", "R1", "ABC", "r01"} {
		file := buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{bad})
		if _, err := ParseNACHAReturn(file); !errors.Is(err, ErrNACHAInvalidReasonCode) {
			t.Errorf("reason code %q: err = %v, want ErrNACHAInvalidReasonCode", bad, err)
		}
	}
	// The boundary codes are valid.
	for _, good := range []string{"R01", "R09", "R10", "R79", "R80", "R85"} {
		file := buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{good})
		returns, err := ParseNACHAReturn(file)
		if err != nil {
			t.Errorf("reason code %q: unexpected err %v", good, err)
		} else if len(returns) != 1 || returns[0].ReasonCode != good {
			t.Errorf("reason code %q: parsed %d returns", good, len(returns))
		}
	}
}

func TestParseNACHAReturn_BadLine(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-3", 100_000)
	file := buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R01"})
	lines := strings.Split(file, "\n")
	lines[2] = lines[2][:90] // truncate the entry detail to 90 chars
	if _, err := ParseNACHAReturn(strings.Join(lines, "\n")); !errors.Is(err, ErrNACHAReturnLine) {
		t.Errorf("truncated line: err = %v, want ErrNACHAReturnLine", err)
	}
	// An orphan addenda (no preceding entry detail) also fails loud.
	if _, err := ParseNACHAReturn(buildAddenda98(t, strings.Split(file, "\n")[2], "R01")); !errors.Is(err, ErrNACHAReturnLine) {
		t.Errorf("orphan addenda: err = %v, want ErrNACHAReturnLine", err)
	}
}

func TestParseNACHAReturn_EntryWithout98Skipped(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-4", 100_000)
	entry, err := PayoutToNACHAEntry(p, achReturnReceiver())
	if err != nil {
		t.Fatalf("PayoutToNACHAEntry: %v", err)
	}
	// A plain origination file: no 98 addenda anywhere.
	file, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{{
		Config: NACHABatchConfig{
			ServiceClassCode:        "220",
			CompanyName:             "ACME CORP",
			CompanyID:               "1122334455",
			EntryClassCode:          "CCD",
			CompanyEntryDescription: "VENDOR PAY",
			EffectiveEntryDate:      time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
			OriginatingDFI:          "12104288",
			BatchNumber:             1,
		},
		Entries: []NACHAEntry{entry},
	}})
	if err != nil {
		t.Fatalf("GenerateNACHAFile: %v", err)
	}
	returns, err := ParseNACHAReturn(file)
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	if len(returns) != 0 {
		t.Fatalf("returns = %d, want 0 (no 98 addenda)", len(returns))
	}
}

func TestParseNACHAReturn_CRLFAndPadding(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-5", 100_000)
	file := buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R02"})
	// CRLF line endings and block padding are both tolerated.
	file = strings.ReplaceAll(file, "\n", "\r\n")
	file += "\r\n" + strings.Repeat("9", 94) + "\r\n" + strings.Repeat("9", 94) + "\r\n"
	returns, err := ParseNACHAReturn(file)
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	if len(returns) != 1 || returns[0].ReasonCode != "R02" {
		t.Fatalf("returns = %+v, want one R02", returns)
	}
}

func TestProcessACHReturn_HappyPath(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := seedPayoutLedger(t, WithAuditLog(al))
	p := seedSubmittedPayout(t, l, "po-ret-6", 250_000)
	if b := l.Balance("saver"); b != 750_000 {
		t.Fatalf("saver balance after submit = %d, want 750000", b)
	}

	returns, err := ParseNACHAReturn(buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R01"}))
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	results, err := l.ProcessACHReturns(returns, "ops")
	if err != nil {
		t.Fatalf("ProcessACHReturns: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first processing")
	}
	wantReversalID := "payout/po-ret-6/clearing/reversal"
	if res.ReversalID != wantReversalID {
		t.Errorf("ReversalID = %q, want %q", res.ReversalID, wantReversalID)
	}
	if res.PayoutID != "po-ret-6" {
		t.Errorf("PayoutID = %q, want po-ret-6", res.PayoutID)
	}
	// The reversal nets the clearing: the payee is whole again, the
	// clearing account is empty.
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance after return = %d, want 1000000", b)
	}
	if b := l.Balance("payout-clearing"); b != 0 {
		t.Errorf("clearing balance after return = %d, want 0", b)
	}
	// The reversal is a genuine reversal of the clearing entry.
	var rev *JournalEntry
	for _, e := range l.Entries() {
		if e.ID == wantReversalID {
			e := e
			rev = &e
			break
		}
	}
	if rev == nil {
		t.Fatalf("reversal entry %q not journaled", wantReversalID)
	}
	if rev.Memo != "reversal_of:payout/po-ret-6/clearing" {
		t.Errorf("reversal memo = %q", rev.Memo)
	}
	// The audit chain carries the ach_return domain event.
	if err := al.Close(); err != nil {
		t.Fatalf("audit close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var found *AuditEvent
	for i, ev := range events {
		if ev.Op == "ach_return" {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no ach_return audit event")
	}
	if found.Actor != "ops" {
		t.Errorf("ach_return actor = %q, want ops", found.Actor)
	}
	if found.Details["reason_code"] != "R01" || found.Details["payout_id"] != "po-ret-6" ||
		found.Details["reversal_id"] != wantReversalID {
		t.Errorf("ach_return details = %v", found.Details)
	}
	// The per-reason-code counter moved exactly once.
	if n := l.ACHReturnCounts()["R01"]; n != 1 {
		t.Errorf("ACHReturnCounts[R01] = %d, want 1", n)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after return: %v", err)
	}
}

func TestProcessACHReturn_IdempotentReplay(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-7", 250_000)
	returns, err := ParseNACHAReturn(buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R01"}))
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	entriesBefore := len(l.Entries())
	first, err := l.ProcessACHReturns(returns, "ops")
	if err != nil {
		t.Fatalf("first ProcessACHReturns: %v", err)
	}
	if first[0].Err != nil || first[0].Duplicate {
		t.Fatalf("first result = %+v", first[0])
	}
	// Replay the same file: duplicate, no new journal rows, no new
	// audit events, no metric bump.
	second, err := l.ProcessACHReturns(returns, "ops")
	if err != nil {
		t.Fatalf("second ProcessACHReturns: %v", err)
	}
	if second[0].Err != nil {
		t.Fatalf("replay result err = %v", second[0].Err)
	}
	if !second[0].Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	if second[0].ReversalID != first[0].ReversalID {
		t.Errorf("replay ReversalID = %q, want %q", second[0].ReversalID, first[0].ReversalID)
	}
	if n := len(l.Entries()); n != entriesBefore+1 {
		t.Errorf("journal entries = %d, want %d (no new row on replay)", n, entriesBefore+1)
	}
	if n := l.ACHReturnCounts()["R01"]; n != 1 {
		t.Errorf("ACHReturnCounts[R01] = %d, want 1 (replay not counted)", n)
	}
}

func TestProcessACHReturn_ReturnOfReturn(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-8", 250_000)
	first, err := ParseNACHAReturn(buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R01"}))
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	if _, err := l.ProcessACHReturns(first, "ops"); err != nil {
		t.Fatalf("first ProcessACHReturns: %v", err)
	}
	// The bank answers again with a different reason code for the same
	// entry — a return of a return (NACHA forbids it, and the ledger
	// cannot correct twice): explicit rejection, not a silent skip.
	second, err := ParseNACHAReturn(buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R02"}))
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	results, err := l.ProcessACHReturns(second, "ops")
	if err != nil {
		t.Fatalf("second ProcessACHReturns: %v", err)
	}
	if !errors.Is(results[0].Err, ErrACHReturnOfReturn) {
		t.Errorf("second return err = %v, want ErrACHReturnOfReturn", results[0].Err)
	}
	if n := l.ACHReturnCounts()["R02"]; n != 0 {
		t.Errorf("ACHReturnCounts[R02] = %d, want 0", n)
	}
	// Balances are untouched by the rejected second return.
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance = %d, want 1000000", b)
	}
}

func TestProcessACHReturn_AmountMismatch(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-9", 250_000)
	file := buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R01"})
	// Tamper with the return file's amount: the return now describes
	// $2,499.99, not the $2,500.00 the clearing entry booked.
	lines := strings.Split(file, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "6") {
			lines[i] = ln[:29] + "0000249999" + ln[39:]
		}
	}
	returns, err := ParseNACHAReturn(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	results, err := l.ProcessACHReturns(returns, "ops")
	if err != nil {
		t.Fatalf("ProcessACHReturns: %v", err)
	}
	if !errors.Is(results[0].Err, ErrACHReturnAmountMismatch) {
		t.Errorf("result err = %v, want ErrACHReturnAmountMismatch", results[0].Err)
	}
	if b := l.Balance("saver"); b != 750_000 {
		t.Errorf("saver balance = %d, want 750000 (nothing booked)", b)
	}
}

func TestProcessACHReturn_UnknownPayout(t *testing.T) {
	l := seedPayoutLedger(t)
	good := seedSubmittedPayout(t, l, "po-ret-10", 100_000)
	ghost := PayoutInstruction{ID: "no-such-payout", AmountCents: 50_000}
	file := buildNACHAReturnFile(t, []PayoutInstruction{good, ghost}, []string{"R01", "R02"})
	returns, err := ParseNACHAReturn(file)
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	results, err := l.ProcessACHReturns(returns, "ops")
	if err != nil {
		t.Fatalf("ProcessACHReturns: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	// Per-item semantics: the good return books, the ghost is a
	// recorded error — neither blocks the other.
	if results[0].Err != nil {
		t.Errorf("good return err = %v", results[0].Err)
	}
	if !errors.Is(results[1].Err, ErrACHReturnPayoutNotFound) {
		t.Errorf("ghost return err = %v, want ErrACHReturnPayoutNotFound", results[1].Err)
	}
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance = %d, want 1000000 (good return booked, ghost booked nothing)", b)
	}
}

func TestProcessACHReturn_NotSubmitted(t *testing.T) {
	l := seedPayoutLedger(t)
	// Draft + approve, but never submit: no clearing entry to correct.
	draftPayout(t, l, "po-ret-11", 100_000)
	if _, err := l.ApprovePayout("po-ret-11", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	p, _ := l.GetPayout("po-ret-11")
	returns, err := ParseNACHAReturn(buildNACHAReturnFile(t, []PayoutInstruction{p}, []string{"R01"}))
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	results, err := l.ProcessACHReturns(returns, "ops")
	if err != nil {
		t.Fatalf("ProcessACHReturns: %v", err)
	}
	if !errors.Is(results[0].Err, ErrACHReturnPayoutNotSubmitted) {
		t.Errorf("result err = %v, want ErrACHReturnPayoutNotSubmitted", results[0].Err)
	}
	if len(l.Entries()) != 1 { // only the seed posting
		t.Errorf("journal entries = %d, want 1 (nothing booked)", len(l.Entries()))
	}
}

func TestProcessACHReturn_Multiple98Addenda(t *testing.T) {
	l := seedPayoutLedger(t)
	p := seedSubmittedPayout(t, l, "po-ret-12", 100_000)
	entry, err := PayoutToNACHAEntry(p, achReturnReceiver())
	if err != nil {
		t.Fatalf("PayoutToNACHAEntry: %v", err)
	}
	file, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{{
		Config: NACHABatchConfig{
			ServiceClassCode:        "220",
			CompanyName:             "ACME CORP",
			CompanyID:               "1122334455",
			EntryClassCode:          "CCD",
			CompanyEntryDescription: "VENDOR PAY",
			EffectiveEntryDate:      time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
			OriginatingDFI:          "12104288",
			BatchNumber:             1,
		},
		Entries: []NACHAEntry{entry},
	}})
	if err != nil {
		t.Fatalf("GenerateNACHAFile: %v", err)
	}
	// One entry, two 98 addenda (the bank attached two return notices).
	var out []string
	for _, ln := range strings.Split(file, "\n") {
		out = append(out, ln)
		if strings.HasPrefix(ln, "6") {
			out = append(out, buildAddenda98(t, ln, "R01"))
			out = append(out, buildAddenda98(t, ln, "R02"))
		}
	}
	returns, err := ParseNACHAReturn(strings.Join(out, "\n"))
	if err != nil {
		t.Fatalf("ParseNACHAReturn: %v", err)
	}
	if len(returns) != 2 {
		t.Fatalf("returns = %d, want 2", len(returns))
	}
	results, err := l.ProcessACHReturns(returns, "ops")
	if err != nil {
		t.Fatalf("ProcessACHReturns: %v", err)
	}
	// The first return books; the second is a return-of-return on the
	// same clearing entry — explicit rejection.
	if results[0].Err != nil {
		t.Errorf("first return err = %v", results[0].Err)
	}
	if !errors.Is(results[1].Err, ErrACHReturnOfReturn) {
		t.Errorf("second return err = %v, want ErrACHReturnOfReturn", results[1].Err)
	}
}
