package ledger

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestSetStatementMatchWindowInvalidRejected pins the fail-closed
// gate: a non-positive window is rejected and the live window is
// left untouched. Zero is rejected, not a "close the window" state
// — zero in the import options already means "take the default", so
// a setter that accepted zero would be ambiguous (along LG-336).
func TestSetStatementMatchWindowInvalidRejected(t *testing.T) {
	l := New()
	if got := l.StatementMatchWindow(); got != DefaultOFXMatchWindow {
		t.Fatalf("default window = %v, want %v", got, DefaultOFXMatchWindow)
	}
	for _, d := range []time.Duration{0, -time.Second, -24 * time.Hour} {
		if err := l.SetStatementMatchWindow(d); !errors.Is(err, ErrInvalidStatementMatchWindow) {
			t.Fatalf("SetStatementMatchWindow(%v) err = %v, want ErrInvalidStatementMatchWindow", d, err)
		}
	}
	// The Ctx variant rejects the same way.
	if err := l.SetStatementMatchWindowCtx(context.Background(), 0); !errors.Is(err, ErrInvalidStatementMatchWindow) {
		t.Fatalf("SetStatementMatchWindowCtx(0) err = %v, want ErrInvalidStatementMatchWindow", err)
	}
	if got := l.StatementMatchWindow(); got != DefaultOFXMatchWindow {
		t.Fatalf("window after rejected revisions = %v, want %v", got, DefaultOFXMatchWindow)
	}
}

// TestSetStatementMatchWindowSameValueNoop pins the no-op: revising
// to the current window returns nil and writes no audit event.
func TestSetStatementMatchWindowSameValueNoop(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetStatementMatchWindow(DefaultOFXMatchWindow); err != nil {
		t.Fatalf("SetStatementMatchWindow(%v) same value: %v", DefaultOFXMatchWindow, err)
	}
	waitAuditWritten(t, al, 1) // the log's startup alert only
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, auditDir)["statement_match_window_revised"]); n != 0 {
		t.Fatalf("same-value revision wrote %d events, want 0", n)
	}
}

// TestSetStatementMatchWindowGetterFollowsLiveValue pins that the
// getter tracks the live value through repeated revisions in both
// directions, including a return to the default.
func TestSetStatementMatchWindowGetterFollowsLiveValue(t *testing.T) {
	l := New()
	for _, d := range []time.Duration{2 * time.Hour, 72 * time.Hour, 96 * time.Hour, 24 * time.Hour} {
		if err := l.SetStatementMatchWindow(d); err != nil {
			t.Fatalf("SetStatementMatchWindow(%v): %v", d, err)
		}
		if got := l.StatementMatchWindow(); got != d {
			t.Fatalf("getter after revision to %v = %v", d, got)
		}
	}
}

// TestSetStatementMatchWindowAuditDetailsExact pins the audit
// payload: exactly the two duration strings, previous and current
// — no statement content ever rides in a retune event.
func TestSetStatementMatchWindowAuditDetailsExact(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetStatementMatchWindow(48 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(48h): %v", err)
	}
	if err := l.SetStatementMatchWindow(12 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(12h): %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["statement_match_window_revised"]
	if len(retuned) != 2 {
		t.Fatalf("statement_match_window_revised events = %d, want 2", len(retuned))
	}
	want := []map[string]any{
		{"previous_window": "24h0m0s", "current_window": "48h0m0s"},
		{"previous_window": "48h0m0s", "current_window": "12h0m0s"},
	}
	for i, ev := range retuned {
		if len(ev.Details) != 2 {
			t.Fatalf("retune[%d] details keys = %v, want exactly previous_window + current_window", i, ev.Details)
		}
		for k, v := range want[i] {
			if ev.Details[k] != v {
				t.Errorf("retune[%d] details[%q] = %v, want %v", i, k, ev.Details[k], v)
			}
		}
		if ev.Actor != "system" {
			t.Errorf("retune[%d] actor = %q, want system (plain setter default)", i, ev.Actor)
		}
	}
}

// TestSetStatementMatchWindowCtxActorAttribution pins the
// attribution on the Ctx variant: the operator identity replaces
// the default actor, while the plain setter keeps the "system"
// default.
func TestSetStatementMatchWindowCtxActorAttribution(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-match")
	if err := l.SetStatementMatchWindowCtx(ctx, 48*time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindowCtx(48h): %v", err)
	}
	if err := l.SetStatementMatchWindow(12 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(12h): %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["statement_match_window_revised"]
	if len(retuned) != 2 {
		t.Fatalf("statement_match_window_revised events = %d, want 2", len(retuned))
	}
	if retuned[0].Actor != "ops-match" {
		t.Errorf("Ctx retune actor = %q, want ops-match", retuned[0].Actor)
	}
	if retuned[1].Actor != "system" {
		t.Errorf("plain retune actor = %q, want system", retuned[1].Actor)
	}
}

// TestStatementMatchWindowGovernsOFXImport pins that the OFX import
// reads the live default when the caller leaves MatchWindow unset:
// a row 48h from the statement date is unmatched under the 24h
// default and matched once the default widens to 72h.
func TestStatementMatchWindowGovernsOFXImport(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "w-1", "cash", "equity", 5000, "USD", base.Add(-48*time.Hour))
	doc := func(fitid string) string {
		return ofxSGMLDoc("cash", "USD",
			"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>50.00\n<FITID>"+fitid)
	}
	rep, err := l.ImportOFX("cash", []byte(doc("win-1")), OFXImportOptions{})
	if err != nil {
		t.Fatalf("ImportOFX: %v", err)
	}
	if rep.Unmatched != 1 {
		t.Fatalf("default window: %+v, want unmatched", rep)
	}
	if err := l.SetStatementMatchWindow(72 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(72h): %v", err)
	}
	rep, err = l.ImportOFX("cash", []byte(doc("win-2")), OFXImportOptions{})
	if err != nil {
		t.Fatalf("ImportOFX after widening: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedEntryID != "w-1" {
		t.Fatalf("72h window: %+v, want matched to w-1", rep.Items[0])
	}
}

// TestStatementMatchWindowGovernsCAMT053Import is the camt.053
// counterpart: a row 60h from the booking date is unmatched under
// the 24h default and matched once the default widens to 96h.
func TestStatementMatchWindowGovernsCAMT053Import(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	acct := AccountID("DE89370400440532013000")
	mustPostOFX(t, l, "w-1", acct, "equity", 4242, "EUR", base.Add(-72*time.Hour))
	ntry := "        <Amt Ccy=\"EUR\">42.42</Amt>\n        <CdtDbtInd>CRDT</CdtDbtInd>\n        <BookgDt><Dt>" + base.Format("2006-01-02") + "</Dt></BookgDt>\n"
	doc := camt053Doc("STMT-W", "IBAN:DE89370400440532013000", ntry)
	rep, err := l.ImportCAMT053(acct, []byte(doc), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT053: %v", err)
	}
	if rep.Unmatched != 1 {
		t.Fatalf("default window: %+v, want unmatched", rep)
	}
	if err := l.SetStatementMatchWindow(96 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(96h): %v", err)
	}
	// A new statement ID keeps the document out of the duplicate
	// registry so the second import re-runs the matcher.
	doc2 := camt053Doc("STMT-W2", "IBAN:DE89370400440532013000", ntry)
	rep, err = l.ImportCAMT053(acct, []byte(doc2), CAMT053ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT053 after widening: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedEntryID != "w-1" {
		t.Fatalf("96h window: %+v, want matched to w-1", rep.Items[0])
	}
}

// TestStatementMatchWindowGovernsCAMT054Import pins that the
// camt.054 triple match reads the live default: a transfer created
// ~3 days before the notification's booking date is
// date_out_of_window under the 24h default and matched once the
// default widens to 96h.
func TestStatementMatchWindowGovernsCAMT054Import(t *testing.T) {
	l := New()
	// Originate the transfer the fixture notification's EndToEndId
	// ("xfer-camt054-1") names.
	if _, err := l.PostTransfer(Transfer{
		ID:             "xfer-camt054-1",
		From:           "acct-payer",
		To:             "acct-notify",
		AmountCents:    50000,
		Currency:       "USD",
		IdempotencyKey: "key-camt054-1",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// The notification books ~3 days before the transfer was
	// created: outside the 24h default, inside a 96h window.
	// (BookgDt is 09:30 on that date, so the delta lands between
	// ~62h and ~87h whatever the time of day.)
	doc := camt054DocOn(time.Now().UTC().Add(-72 * time.Hour).Format("2006-01-02"))
	rep, err := l.ImportCAMT054("acct-notify", []byte(doc), CAMT054ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT054: %v", err)
	}
	if len(rep.Items) == 0 || rep.Items[0].Status != CAMT054Mismatched || rep.Items[0].MismatchReason != "date_out_of_window" {
		t.Fatalf("default window: first item = %+v, want mismatched/date_out_of_window", rep.Items[0])
	}
	if err := l.SetStatementMatchWindow(96 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(96h): %v", err)
	}
	// A different notification date keeps the document out of the
	// duplicate registry so the second import re-runs the matcher
	// (its booking date is ~2 days before the transfer — still
	// outside the 24h default, inside the 96h window).
	doc2 := camt054DocOn(time.Now().UTC().Add(-48 * time.Hour).Format("2006-01-02"))
	rep, err = l.ImportCAMT054("acct-notify", []byte(doc2), CAMT054ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT054 after widening: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedID != "xfer-camt054-1" {
		t.Fatalf("96h window: %+v, want matched to xfer-camt054-1", rep.Items[0])
	}
}

// TestStatementMatchWindowExplicitOptionWins pins that an explicit
// opts.MatchWindow always beats the live default — in both
// directions: a tight live default does not shrink an explicit wide
// window, and the live default governs when the caller leaves the
// window unset.
func TestStatementMatchWindowExplicitOptionWins(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "w-1", "cash", "equity", 5000, "USD", base.Add(-48*time.Hour))
	doc := func(fitid string) string {
		return ofxSGMLDoc("cash", "USD",
			"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>50.00\n<FITID>"+fitid)
	}
	if err := l.SetStatementMatchWindow(2 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(2h): %v", err)
	}
	rep, err := l.ImportOFX("cash", []byte(doc("exp-1")), OFXImportOptions{MatchWindow: 72 * time.Hour})
	if err != nil {
		t.Fatalf("ImportOFX explicit window: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedEntryID != "w-1" {
		t.Fatalf("explicit 72h window under a 2h live default: %+v, want matched to w-1", rep.Items[0])
	}
	rep, err = l.ImportOFX("cash", []byte(doc("exp-2")), OFXImportOptions{})
	if err != nil {
		t.Fatalf("ImportOFX unset window: %v", err)
	}
	if rep.Unmatched != 1 {
		t.Fatalf("unset window under a 2h live default: %+v, want unmatched", rep)
	}
}

// TestStatementMatchWindowGovernsMatchStatement pins that the
// standalone matcher reads the live default: a line 48h from the
// journaled row is unmatched under the 24h default and matched
// once the default widens to 72h.
func TestStatementMatchWindowGovernsMatchStatement(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "m-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 15000, Currency: "USD",
		Memo: "payroll oct", ExternalRef: "PAY-1042", CreatedAt: base,
	})
	doc := statementMatchDoc(base.Add(48*time.Hour).Format("20060102150405"),
		[5]string{"fit-1", "CREDIT", "150.00", "payroll oct", "PAY-1042"})
	txns, err := ParseOFX([]byte(doc))
	if err != nil {
		t.Fatalf("ParseOFX: %v", err)
	}
	line := NewStatementLineFromOFX(txns[0], "docsha-1")
	rep, err := l.MatchStatement("cash", []StatementLine{line}, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	if rep.Unmatched != 1 {
		t.Fatalf("default window: %+v, want unmatched", rep)
	}
	if err := l.SetStatementMatchWindow(72 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(72h): %v", err)
	}
	rep, err = l.MatchStatement("cash", []StatementLine{line}, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement after widening: %v", err)
	}
	if rep.Matched != 1 || rep.Items[0].MatchedEntryID != "m-1" {
		t.Fatalf("72h window: %+v, want matched to m-1", rep.Items[0])
	}
}

// TestStatementMatchWindowGovernsNostroMatch pins that nostro
// reconciliation reads the live default on its fuzzy path (identity
// matches ignore the window by design, so the line carries no
// external reference): an expected line 48h from the journaled row
// is unreached under the 24h default and matched once the default
// widens to 72h.
func TestStatementMatchWindowGovernsNostroMatch(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)
	postNostroEntry(t, l, "e-1", "nostro-usd", true, 40_000, "")
	lines := []StatementLine{nostroTestLine("ln-1", "nostro-usd", StatementDirectionIn, 40_000, now.Add(-48*time.Hour), "")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Matched != 0 || rep.ExpectedUnreached != 1 {
		t.Fatalf("default window: matched=%d unreached=%d, want 0/1", rep.Matched, rep.ExpectedUnreached)
	}
	if err := l.SetStatementMatchWindow(72 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(72h): %v", err)
	}
	rep, err = l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro after widening: %v", err)
	}
	if rep.Matched != 1 || rep.ExpectedUnreached != 0 {
		t.Fatalf("72h window: matched=%d unreached=%d, want 1/0", rep.Matched, rep.ExpectedUnreached)
	}
}

// TestStatementMatchWindowConcurrentRevisionAndImport pins that
// concurrent revisions race cleanly with concurrent imports: every
// window read is under l.mu, so the race detector is the assertion
// — either match outcome for any single import is legitimate under
// a moving window.
func TestStatementMatchWindowConcurrentRevisionAndImport(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "w-1", "cash", "equity", 5000, "USD", base.Add(-48*time.Hour))
	windows := []time.Duration{2 * time.Hour, 24 * time.Hour, 72 * time.Hour, 96 * time.Hour}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = l.SetStatementMatchWindow(windows[(g+i)%len(windows)])
			}
		}(g)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				// A distinct FITID keeps each document out of the
				// duplicate registry so every call re-runs the
				// matcher against the live window.
				doc := ofxSGMLDoc("cash", "USD",
					"<TRNTYPE>CREDIT\n<DTPOSTED>"+base.Format("20060102150405")+"\n<TRNAMT>50.00\n<FITID>race-"+string(rune('a'+i))+string(rune('0'+j)))
				_, _ = l.ImportOFX("cash", []byte(doc), OFXImportOptions{})
				_ = l.StatementMatchWindow()
			}
		}(i)
	}
	wg.Wait()
	got := l.StatementMatchWindow()
	for _, w := range windows {
		if got == w {
			return
		}
	}
	t.Errorf("live window = %v, want one of %v", got, windows)
}

// TestStatementMatchWindowSnapshotRoundTripRestoresDefault pins
// that the window does not round-trip through snapshots: an
// export/import restores the 24h default.
func TestStatementMatchWindowSnapshotRoundTripRestoresDefault(t *testing.T) {
	l := New()
	if err := l.SetStatementMatchWindow(72 * time.Hour); err != nil {
		t.Fatalf("SetStatementMatchWindow(72h): %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.StatementMatchWindow(); got != DefaultOFXMatchWindow {
		t.Fatalf("imported window = %v, want default %v", got, DefaultOFXMatchWindow)
	}
}
