package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// FX spread-income account amendment (LG-296): the runtime revision of
// the ledger-wide income account, its fail-closed gates, and the audit
// trail. The posting paths already read l.fxIncomeAccount live, so the
// tests pin that the revision — not a posting change — moves the spread
// leg.

// amendTestLedger builds a spread ledger with an audit log: USD->EUR
// mid 110/100, bid 108/100, ask 112/100, FX clearing account "fx-pnl",
// FX income account "fx-inc", and a funded payer. The caller must not
// use the audit log after amendAuditEvents closes it.
func amendTestLedger(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al), WithFXAccount("fx-pnl"), WithFXIncomeAccount("fx-inc"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 110, 100)
	mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 112, 100)
	return l, al
}

// amendAuditEvents returns the fx_income_account_amended events in
// order, closing the audit log first.
func amendAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_income_account_amended" {
			out = append(out, e)
		}
	}
	return out
}

func postSpreadTransfer(t *testing.T, l *Ledger, id string) TransferReceipt {
	t.Helper()
	rcpt, err := l.PostTransfer(Transfer{
		ID: id, From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer(%s): %v", id, err)
	}
	return rcpt
}

func TestSetFXIncomeAccountAmendmentAffectsNextTransfer(t *testing.T) {
	l, al := amendTestLedger(t)
	v := l.Version()

	if err := l.SetFXIncomeAccount("fx-inc-2"); err != nil {
		t.Fatalf("SetFXIncomeAccount: %v", err)
	}
	if got := l.FXIncomeAccount(); got != "fx-inc-2" {
		t.Errorf("FXIncomeAccount() = %q, want fx-inc-2", got)
	}
	if got := l.Version(); got != v {
		t.Errorf("Version after amendment = %d, want %d (structural, no bump)", got, v)
	}

	// The next spread-earning transfer credits the new account:
	// sell 10000 USD at bid 108/100 against mid 110/100 earns
	// floor(10000*110/100) - floor(10000*108/100) = 200 EUR.
	rcpt := postSpreadTransfer(t, l, "am1")
	if rcpt.FX == nil || rcpt.FX.SpreadCents != 200 {
		t.Fatalf("spread disclosure: %+v, want 200", rcpt.FX)
	}
	if rcpt.FX.FXIncomeAccount != "fx-inc-2" {
		t.Errorf("receipt income account = %q, want fx-inc-2", rcpt.FX.FXIncomeAccount)
	}
	found := false
	for _, e := range rcpt.Entries {
		if e.ID == "am1/spread" {
			found = true
			if e.DebitAccount != "fx-pnl" || e.CreditAccount != "fx-inc-2" ||
				e.AmountCents != 200 || e.Currency != "EUR" {
				t.Errorf("bad spread leg after amendment: %+v", e)
			}
		}
	}
	if !found {
		t.Error("spread leg missing after amendment")
	}
	// The old income account booked nothing.
	if got := l.BalanceIn("fx-inc", "EUR"); got != 0 {
		t.Errorf("old income account EUR = %d, want 0", got)
	}

	events := amendAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("fx_income_account_amended events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetFXIncomeAccount" {
		t.Errorf("actor = %q, want SetFXIncomeAccount", ev.Actor)
	}
	if ev.Details["previous_income_account"] != "fx-inc" || ev.Details["income_account"] != "fx-inc-2" {
		t.Errorf("audit details = %+v, want previous fx-inc, new fx-inc-2", ev.Details)
	}
	if ev.VersionBefore != v || ev.VersionAfter != v {
		t.Errorf("audit versions = %d/%d, want %d/%d", ev.VersionBefore, ev.VersionAfter, v, v)
	}
}

func TestSetFXIncomeAccountSameValueNoOp(t *testing.T) {
	l, al := amendTestLedger(t)
	v := l.Version()
	if err := l.SetFXIncomeAccount("fx-inc"); err != nil {
		t.Fatalf("same-value revision: %v", err)
	}
	if got := l.FXIncomeAccount(); got != "fx-inc" {
		t.Errorf("FXIncomeAccount() = %q, want fx-inc", got)
	}
	if got := l.Version(); got != v {
		t.Errorf("Version after no-op = %d, want %d", got, v)
	}
	if events := amendAuditEvents(t, al); len(events) != 0 {
		t.Errorf("same-value revision wrote %d audit events, want 0", len(events))
	}
}

func TestSetFXIncomeAccountEmptyClears(t *testing.T) {
	l, _ := amendTestLedger(t)
	if err := l.SetFXIncomeAccount(""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got := l.FXIncomeAccount(); got != "" {
		t.Errorf("FXIncomeAccount() = %q, want empty", got)
	}
	// The next positive-spread transfer fails closed, recording nothing.
	v := l.Version()
	if _, err := l.PostTransfer(Transfer{
		ID: "am2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	}); !errors.Is(err, ErrFXIncomeAccountNotConfigured) {
		t.Errorf("want ErrFXIncomeAccountNotConfigured, got %v", err)
	}
	if l.Version() != v {
		t.Error("rejected transfer bumped the version")
	}
	if _, exists := l.entries["am2"]; exists {
		t.Error("rejected transfer journaled entries")
	}
}

func TestSetFXIncomeAccountEqualsFXAccountRejected(t *testing.T) {
	l, al := amendTestLedger(t)
	if err := l.SetFXIncomeAccount("fx-pnl"); !errors.Is(err, ErrInvalidFXIncomeAccount) {
		t.Errorf("want ErrInvalidFXIncomeAccount, got %v", err)
	}
	// The original value is untouched.
	if got := l.FXIncomeAccount(); got != "fx-inc" {
		t.Errorf("FXIncomeAccount() = %q, want fx-inc (unchanged)", got)
	}
	// The next transfer still credits the original account.
	rcpt := postSpreadTransfer(t, l, "am3")
	if rcpt.FX.FXIncomeAccount != "fx-inc" {
		t.Errorf("receipt income account = %q, want fx-inc", rcpt.FX.FXIncomeAccount)
	}
	if events := amendAuditEvents(t, al); len(events) != 0 {
		t.Errorf("rejected revision wrote %d audit events, want 0", len(events))
	}
}

func TestSetFXIncomeAccountPerTransferOverrideStillWins(t *testing.T) {
	l, _ := amendTestLedger(t)
	if err := l.SetFXIncomeAccount("fx-inc-2"); err != nil {
		t.Fatalf("SetFXIncomeAccount: %v", err)
	}
	rcpt, err := l.PostTransfer(Transfer{
		ID: "am4", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", FXIncomeAccount: "desk-inc",
	})
	if err != nil {
		t.Fatalf("PostTransfer with override: %v", err)
	}
	if rcpt.FX.FXIncomeAccount != "desk-inc" {
		t.Errorf("receipt income account = %q, want desk-inc (override wins)", rcpt.FX.FXIncomeAccount)
	}
	for _, e := range rcpt.Entries {
		if e.ID == "am4/spread" && e.CreditAccount != "desk-inc" {
			t.Errorf("spread leg credits %q, want desk-inc", e.CreditAccount)
		}
	}
}

func TestSetFXIncomeAccountSplitReadsLiveValue(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"), WithFXIncomeAccount("fx-inc"), WithMaxTransferChunkCents(4000))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 110, 100)
	mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 112, 100)
	if err := l.SetFXIncomeAccount("fx-inc-2"); err != nil {
		t.Fatalf("SetFXIncomeAccount: %v", err)
	}
	rcpt, err := l.PostTransferSplit(Transfer{
		ID: "am5", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	// Per-chunk spread: 2x(4400-4320) + (2200-2160) = 200.
	if rcpt.FX == nil || rcpt.FX.SpreadCents != 200 {
		t.Fatalf("split spread disclosure: %+v, want 200", rcpt.FX)
	}
	found := false
	for _, e := range rcpt.Entries {
		if e.ID == "am5/spread" {
			found = true
			if e.CreditAccount != "fx-inc-2" {
				t.Errorf("split spread leg credits %q, want fx-inc-2", e.CreditAccount)
			}
		}
	}
	if !found {
		t.Error("split spread leg missing after amendment")
	}
}

func TestSetFXIncomeAccountSnapshotRoundTrip(t *testing.T) {
	l := spreadTestLedger(t)
	if err := l.SetFXIncomeAccount("fx-inc-2"); err != nil {
		t.Fatalf("SetFXIncomeAccount: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.FXIncomeAccount(); got != "fx-inc-2" {
		t.Errorf("restored FXIncomeAccount() = %q, want fx-inc-2", got)
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Error("restored ledger differs (amended income account must round-trip)")
	}
}

func TestSetFXIncomeAccountCtxRecordsOperator(t *testing.T) {
	l, al := amendTestLedger(t)
	ctx := ContextWithActor(context.Background(), "operator-alice")
	if err := l.SetFXIncomeAccountCtx(ctx, "fx-inc-2"); err != nil {
		t.Fatalf("SetFXIncomeAccountCtx: %v", err)
	}
	events := amendAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("fx_income_account_amended events = %d, want 1", len(events))
	}
	if events[0].Actor != "operator-alice" {
		t.Errorf("actor = %q, want operator-alice", events[0].Actor)
	}
}
