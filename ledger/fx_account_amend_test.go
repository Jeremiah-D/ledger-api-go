package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// FX clearing account amendment (LG-297): the runtime revision of the
// ledger-wide clearing account, its fail-closed gates, and the audit
// trail. The posting paths already read l.fxAccount live, so the tests
// pin that the revision — not a posting change — moves the clearing
// legs. amendTestLedger (LG-296) supplies the fixture: USD->EUR mid
// 110/100, bid 108/100, ask 112/100, FX clearing account "fx-pnl", FX
// income account "fx-inc", and a funded payer.

// fxAccountAmendAuditEvents returns the fx_account_amended events in
// order, closing the audit log first.
func fxAccountAmendAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_account_amended" {
			out = append(out, e)
		}
	}
	return out
}

func TestSetFXAccountAmendmentAffectsNextTransfer(t *testing.T) {
	l, al := amendTestLedger(t)
	v := l.Version()

	if err := l.SetFXAccount("fx-pnl-2"); err != nil {
		t.Fatalf("SetFXAccount: %v", err)
	}
	if got := l.FXAccount(); got != "fx-pnl-2" {
		t.Errorf("FXAccount() = %q, want fx-pnl-2", got)
	}
	if got := l.Version(); got != v {
		t.Errorf("Version after amendment = %d, want %d (structural, no bump)", got, v)
	}

	// The next cross-currency transfer settles its clearing legs
	// through the new account: sell 10000 USD at bid 108/100 converts
	// to 10800 EUR, with a 200 EUR spread leg.
	rcpt := postSpreadTransfer(t, l, "fxa1")
	if rcpt.FX == nil || rcpt.FX.ConvertedCents != 10800 {
		t.Fatalf("conversion disclosure: %+v, want 10800", rcpt.FX)
	}
	legs := map[string]JournalEntry{}
	for _, e := range rcpt.Entries {
		legs[e.ID] = e
	}
	if leg, ok := legs["fxa1"]; !ok {
		t.Error("principal leg missing after amendment")
	} else if leg.CreditAccount != "fx-pnl-2" || leg.DebitAccount != "payee" ||
		leg.AmountCents != 10800 || leg.Currency != "EUR" {
		t.Errorf("bad principal leg after amendment: %+v", leg)
	}
	if leg, ok := legs["fxa1/fx"]; !ok {
		t.Error("fx leg missing after amendment")
	} else if leg.DebitAccount != "fx-pnl-2" || leg.CreditAccount != "payer" ||
		leg.AmountCents != 10000 || leg.Currency != "USD" {
		t.Errorf("bad fx leg after amendment: %+v", leg)
	}
	if leg, ok := legs["fxa1/spread"]; !ok {
		t.Error("spread leg missing after amendment")
	} else if leg.DebitAccount != "fx-pnl-2" || leg.CreditAccount != "fx-inc" ||
		leg.AmountCents != 200 || leg.Currency != "EUR" {
		t.Errorf("bad spread leg after amendment: %+v", leg)
	}
	// The old clearing account booked nothing.
	if got := l.BalanceIn("fx-pnl", "EUR"); got != 0 {
		t.Errorf("old clearing account EUR = %d, want 0", got)
	}
	if got := l.BalanceIn("fx-pnl", "USD"); got != 0 {
		t.Errorf("old clearing account USD = %d, want 0", got)
	}

	events := fxAccountAmendAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("fx_account_amended events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetFXAccount" {
		t.Errorf("actor = %q, want SetFXAccount", ev.Actor)
	}
	if ev.Details["previous_account"] != "fx-pnl" || ev.Details["account"] != "fx-pnl-2" {
		t.Errorf("audit details = %+v, want previous fx-pnl, new fx-pnl-2", ev.Details)
	}
	if ev.VersionBefore != v || ev.VersionAfter != v {
		t.Errorf("audit versions = %d/%d, want %d/%d", ev.VersionBefore, ev.VersionAfter, v, v)
	}
}

func TestSetFXAccountSameValueNoOp(t *testing.T) {
	l, al := amendTestLedger(t)
	v := l.Version()
	if err := l.SetFXAccount("fx-pnl"); err != nil {
		t.Fatalf("same-value revision: %v", err)
	}
	if got := l.FXAccount(); got != "fx-pnl" {
		t.Errorf("FXAccount() = %q, want fx-pnl", got)
	}
	if got := l.Version(); got != v {
		t.Errorf("Version after no-op = %d, want %d", got, v)
	}
	if events := fxAccountAmendAuditEvents(t, al); len(events) != 0 {
		t.Errorf("same-value revision wrote %d audit events, want 0", len(events))
	}
}

func TestSetFXAccountEmptyClears(t *testing.T) {
	l, _ := amendTestLedger(t)
	if err := l.SetFXAccount(""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got := l.FXAccount(); got != "" {
		t.Errorf("FXAccount() = %q, want empty", got)
	}
	// The next cross-currency transfer fails closed, recording nothing.
	v := l.Version()
	if _, err := l.PostTransfer(Transfer{
		ID: "fxa2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	}); !errors.Is(err, ErrFXAccountNotConfigured) {
		t.Errorf("want ErrFXAccountNotConfigured, got %v", err)
	}
	if l.Version() != v {
		t.Error("rejected transfer bumped the version")
	}
	if _, exists := l.entries["fxa2"]; exists {
		t.Error("rejected transfer journaled entries")
	}
}

func TestSetFXAccountEqualsIncomeAccountRejected(t *testing.T) {
	l, al := amendTestLedger(t)
	if err := l.SetFXAccount("fx-inc"); !errors.Is(err, ErrFXAccountEqualsIncomeAccount) {
		t.Errorf("want ErrFXAccountEqualsIncomeAccount, got %v", err)
	}
	// The original value is untouched.
	if got := l.FXAccount(); got != "fx-pnl" {
		t.Errorf("FXAccount() = %q, want fx-pnl (unchanged)", got)
	}
	// The next transfer still settles through the original account.
	rcpt := postSpreadTransfer(t, l, "fxa3")
	for _, e := range rcpt.Entries {
		if e.ID == "fxa3" && e.CreditAccount != "fx-pnl" {
			t.Errorf("principal leg credits %q, want fx-pnl", e.CreditAccount)
		}
		if e.ID == "fxa3/fx" && e.DebitAccount != "fx-pnl" {
			t.Errorf("fx leg debits %q, want fx-pnl", e.DebitAccount)
		}
	}
	if events := fxAccountAmendAuditEvents(t, al); len(events) != 0 {
		t.Errorf("rejected revision wrote %d audit events, want 0", len(events))
	}
}

func TestSetFXAccountPerTransferOverrideStillWins(t *testing.T) {
	l, _ := amendTestLedger(t)
	if err := l.SetFXAccount("fx-pnl-2"); err != nil {
		t.Fatalf("SetFXAccount: %v", err)
	}
	rcpt, err := l.PostTransfer(Transfer{
		ID: "fxa4", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", FXAccount: "desk-fx",
	})
	if err != nil {
		t.Fatalf("PostTransfer with override: %v", err)
	}
	for _, e := range rcpt.Entries {
		if e.ID == "fxa4" && e.CreditAccount != "desk-fx" {
			t.Errorf("principal leg credits %q, want desk-fx (override wins)", e.CreditAccount)
		}
		if e.ID == "fxa4/fx" && e.DebitAccount != "desk-fx" {
			t.Errorf("fx leg debits %q, want desk-fx (override wins)", e.DebitAccount)
		}
	}
}

func TestSetFXAccountSnapshotRoundTrip(t *testing.T) {
	l := spreadTestLedger(t)
	if err := l.SetFXAccount("fx-pnl-2"); err != nil {
		t.Fatalf("SetFXAccount: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.FXAccount(); got != "fx-pnl-2" {
		t.Errorf("restored FXAccount() = %q, want fx-pnl-2", got)
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Error("restored ledger differs (amended clearing account must round-trip)")
	}
}

func TestSetFXAccountCtxRecordsOperator(t *testing.T) {
	l, al := amendTestLedger(t)
	ctx := ContextWithActor(context.Background(), "operator-alice")
	if err := l.SetFXAccountCtx(ctx, "fx-pnl-2"); err != nil {
		t.Fatalf("SetFXAccountCtx: %v", err)
	}
	events := fxAccountAmendAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("fx_account_amended events = %d, want 1", len(events))
	}
	if events[0].Actor != "operator-alice" {
		t.Errorf("actor = %q, want operator-alice", events[0].Actor)
	}
}
