package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestSetTransferAllowlistValidation(t *testing.T) {
	l := New()
	if err := l.SetTransferAllowlist("", []AccountID{"bob"}); !errors.Is(err, ErrInvalidTransferAllowlist) {
		t.Errorf("empty payer: err=%v, want ErrInvalidTransferAllowlist", err)
	}
	if err := l.SetTransferAllowlist("alice", []AccountID{"bob", ""}); !errors.Is(err, ErrInvalidTransferAllowlist) {
		t.Errorf("empty beneficiary: err=%v, want ErrInvalidTransferAllowlist", err)
	}
	if err := l.SetTransferAllowlist("alice", []AccountID{""}); !errors.Is(err, ErrInvalidTransferAllowlist) {
		t.Errorf("single empty beneficiary: err=%v, want ErrInvalidTransferAllowlist", err)
	}
	if err := l.ClearTransferAllowlist(""); !errors.Is(err, ErrInvalidTransferAllowlist) {
		t.Errorf("clear empty payer: err=%v, want ErrInvalidTransferAllowlist", err)
	}
	if _, enabled := l.TransferAllowlistOf("alice"); enabled {
		t.Errorf("gate enabled after rejected sets, want disabled")
	}
}

func TestTransferAllowlistSetGetClear(t *testing.T) {
	l := New()

	if _, enabled := l.TransferAllowlistOf("alice"); enabled {
		t.Fatalf("gate enabled before any set, want disabled")
	}
	// Duplicates collapse; the getter returns a sorted slice.
	if err := l.SetTransferAllowlist("alice", []AccountID{"carol", "bob", "bob"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	allowlist, enabled := l.TransferAllowlistOf("alice")
	if !enabled {
		t.Fatalf("enabled=false after set, want true")
	}
	if len(allowlist) != 2 || allowlist[0] != "bob" || allowlist[1] != "carol" {
		t.Errorf("allowlist=%v, want [bob carol]", allowlist)
	}
	// The getter's slice must not alias the stored set: mutating it
	// must not change the gate.
	allowlist[0] = "mallory"
	again, _ := l.TransferAllowlistOf("alice")
	if again[0] != "bob" {
		t.Errorf("stored allowlist aliased by getter slice: %v", again)
	}

	// Replacing the list installs the new set.
	if err := l.SetTransferAllowlist("alice", []AccountID{"erin"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	allowlist, enabled = l.TransferAllowlistOf("alice")
	if !enabled || len(allowlist) != 1 || allowlist[0] != "erin" {
		t.Errorf("after replace: %v enabled=%v, want [erin] enabled", allowlist, enabled)
	}

	// Setting an empty list clears: the gate disables and the getter
	// reports disabled, like a payer that never had a list.
	if err := l.SetTransferAllowlist("alice", nil); err != nil {
		t.Fatalf("empty set: %v", err)
	}
	if allowlist, enabled := l.TransferAllowlistOf("alice"); enabled || allowlist != nil {
		t.Errorf("after empty set: %v enabled=%v, want nil disabled", allowlist, enabled)
	}

	// Clearing an unset account is a silent no-op.
	if err := l.ClearTransferAllowlist("alice"); err != nil {
		t.Errorf("clear unset: %v, want nil", err)
	}
}

func TestTransferAllowlistAuditTrail(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetTransferAllowlist("alice", []AccountID{"bob"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Replaying the identical list is silent: no second event.
	if err := l.SetTransferAllowlist("alice", []AccountID{"bob"}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// A real change is one event, and a set-with-empty clear emits the
	// clear event.
	if err := l.SetTransferAllowlist("alice", []AccountID{"carol"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := l.SetTransferAllowlist("alice", nil); err != nil {
		t.Fatalf("clear via empty set: %v", err)
	}
	// Clearing an unset account stays silent.
	if err := l.ClearTransferAllowlist("alice"); err != nil {
		t.Fatalf("clear unset: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	found := map[string]int{}
	for _, e := range events {
		found[e.Op]++
	}
	if found["allowlist_set"] != 2 {
		t.Errorf("allowlist_set events = %d, want 2", found["allowlist_set"])
	}
	if found["allowlist_cleared"] != 1 {
		t.Errorf("allowlist_cleared events = %d, want 1", found["allowlist_cleared"])
	}
	// Structural config never bumps the journal version.
	if l.Version() != 0 {
		t.Errorf("version = %d after allowlist changes, want 0", l.Version())
	}
}

func TestTransferAllowlistGate(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	if err := l.SetTransferAllowlist("alice", []AccountID{"bob"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	// A listed payee books normally.
	if _, err := l.PostTransfer(Transfer{ID: "al-tx-1", From: "alice", To: "bob", AmountCents: 2500}); err != nil {
		t.Fatalf("transfer to listed bob: %v", err)
	}

	// An unlisted payee is refused with 403-class ErrBeneficiaryNotAllowed
	// and records nothing: no journal rows, no version bump.
	before := l.Version()
	entriesBefore := len(l.Entries())
	_, err := l.PostTransfer(Transfer{ID: "al-tx-2", From: "alice", To: "mallory", AmountCents: 2500})
	if !errors.Is(err, ErrBeneficiaryNotAllowed) {
		t.Fatalf("transfer to unlisted mallory: err=%v, want ErrBeneficiaryNotAllowed", err)
	}
	if l.Version() != before {
		t.Errorf("version = %d after refused transfer, want %d", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("entries = %d after refused transfer, want %d", len(l.Entries()), entriesBefore)
	}
}

func TestTransferAllowlistDisabledGate(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	// No list installed: transfers may pay anyone, as before allowlists
	// existed.
	if _, err := l.PostTransfer(Transfer{ID: "noal-tx-1", From: "alice", To: "mallory", AmountCents: 100}); err != nil {
		t.Errorf("transfer with no list: %v, want nil", err)
	}
}

func TestTransferAllowlistGateOrder(t *testing.T) {
	// The check order is validation -> idempotency replay -> ID
	// conflict -> frozen -> allowlist -> overdraft -> commit. Each case
	// below pins one precedence relationship.

	t.Run("replay before allowlist", func(t *testing.T) {
		l := New()
		seedBalance(t, l, "alice", 10000)
		orig, err := l.PostTransfer(Transfer{
			ID: "ord-tx-1", From: "alice", To: "bob", AmountCents: 1000,
			IdempotencyKey: "ord-key-1",
		})
		if err != nil {
			t.Fatalf("original: %v", err)
		}
		// Install a list that excludes bob AFTER the original posted.
		// Replaying the key still returns the original receipt — the
		// replay books nothing new, and the gate does not apply to it.
		if err := l.SetTransferAllowlist("alice", []AccountID{"mallory"}); err != nil {
			t.Fatalf("set: %v", err)
		}
		replay, err := l.PostTransfer(Transfer{
			ID: "ord-tx-2", From: "alice", To: "bob", AmountCents: 1000,
			IdempotencyKey: "ord-key-1",
		})
		if err != nil {
			t.Fatalf("replay: err=%v, want the original receipt", err)
		}
		if !replay.Duplicate {
			t.Errorf("replay.Duplicate = false, want true")
		}
		if len(replay.Entries) != len(orig.Entries) || replay.Entries[0].ID != orig.Entries[0].ID {
			t.Errorf("replay entries = %v, want the original entries", replay.Entries)
		}
	})

	t.Run("frozen before allowlist", func(t *testing.T) {
		l := New()
		seedBalance(t, l, "alice", 10000)
		if err := l.SetTransferAllowlist("alice", []AccountID{"mallory"}); err != nil {
			t.Fatalf("set: %v", err)
		}
		l.Freeze("alice")
		// Frozen beats the allowlist gate: the payer is stopped either
		// way, and the error names the stop.
		_, err := l.PostTransfer(Transfer{ID: "ord-tx-3", From: "alice", To: "mallory", AmountCents: 100})
		if !errors.Is(err, ErrAccountFrozen) {
			t.Errorf("frozen+allowlist-miss: err=%v, want ErrAccountFrozen", err)
		}
	})

	t.Run("allowlist before overdraft", func(t *testing.T) {
		l := New()
		l.EnableOverdraftProtection("alice")
		seedBalance(t, l, "alice", 100)
		if err := l.SetTransferAllowlist("alice", []AccountID{"bob"}); err != nil {
			t.Fatalf("set: %v", err)
		}
		// The payee is listed, so the allowlist passes; the protected
		// payer's balance cannot cover the outflow, so overdraft fails.
		_, err := l.PostTransfer(Transfer{ID: "ord-tx-4", From: "alice", To: "bob", AmountCents: 5000})
		if !errors.Is(err, ErrAccountOverdraft) {
			t.Errorf("allowlist-hit+overdraft: err=%v, want ErrAccountOverdraft", err)
		}
	})
}

func TestTransferAllowlistFeeLegExempt(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	// The allowlist names the business payee only; the fee account is
	// the operator's own revenue account and must not need a listing.
	if err := l.SetTransferAllowlist("alice", []AccountID{"bob"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	receipt, err := l.PostTransfer(Transfer{
		ID: "fee-tx-1", From: "alice", To: "bob", AmountCents: 1000,
		FeeCents: 50, FeeAccount: "fee-revenue",
	})
	if err != nil {
		t.Fatalf("transfer with fee leg: %v", err)
	}
	if receipt.FeeCents != 50 {
		t.Errorf("receipt.FeeCents = %d, want 50", receipt.FeeCents)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("receipt.Entries has %d rows, want principal + fee", len(receipt.Entries))
	}
}

func TestTransferAllowlistSnapshotRoundtrip(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	if err := l.SetTransferAllowlist("alice", []AccountID{"bob", "carol"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	allowlist, enabled := restored.TransferAllowlistOf("alice")
	if !enabled || len(allowlist) != 2 || allowlist[0] != "bob" || allowlist[1] != "carol" {
		t.Fatalf("restored allowlist = %v enabled=%v, want [bob carol] enabled", allowlist, enabled)
	}
	// The gate survives the restore.
	if _, err := restored.PostTransfer(Transfer{ID: "rt-tx-1", From: "alice", To: "mallory", AmountCents: 100}); !errors.Is(err, ErrBeneficiaryNotAllowed) {
		t.Errorf("restored transfer to mallory: err=%v, want ErrBeneficiaryNotAllowed", err)
	}
	if _, err := restored.PostTransfer(Transfer{ID: "rt-tx-2", From: "alice", To: "bob", AmountCents: 100}); err != nil {
		t.Errorf("restored transfer to bob: %v", err)
	}
}

func TestTransferAllowlistIncrementalRoundtrip(t *testing.T) {
	a := New()
	seedBalance(t, a, "alice", 10000)
	if err := a.SetTransferAllowlist("alice", []AccountID{"bob"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	base := a.versionOf()

	// The replica at base version, like a real replica built from a full
	// snapshot.
	var full bytes.Buffer
	if err := a.ExportSnapshot(&full); err != nil {
		t.Fatalf("export: %v", err)
	}
	b, err := ImportSnapshot(&full)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// New activity on the source: a transfer and an allowlist change.
	if _, err := a.PostTransfer(Transfer{ID: "inc-tx-1", From: "alice", To: "bob", AmountCents: 100}); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if err := a.SetTransferAllowlist("alice", []AccountID{"carol"}); err != nil {
		t.Fatalf("change: %v", err)
	}

	var delta bytes.Buffer
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatalf("export delta: %v", err)
	}
	if err := b.ImportIncrementalSnapshot(&delta); err != nil {
		t.Fatalf("import delta: %v", err)
	}
	allowlist, enabled := b.TransferAllowlistOf("alice")
	if !enabled || len(allowlist) != 1 || allowlist[0] != "carol" {
		t.Fatalf("replica allowlist = %v enabled=%v, want [carol] enabled", allowlist, enabled)
	}
	// The gate rides the config: the replica now rejects the old
	// beneficiary and accepts the new one.
	if _, err := b.PostTransfer(Transfer{ID: "inc-tx-2", From: "alice", To: "bob", AmountCents: 100}); !errors.Is(err, ErrBeneficiaryNotAllowed) {
		t.Errorf("replica transfer to bob: err=%v, want ErrBeneficiaryNotAllowed", err)
	}
	if _, err := b.PostTransfer(Transfer{ID: "inc-tx-3", From: "alice", To: "carol", AmountCents: 100}); err != nil {
		t.Errorf("replica transfer to carol: %v", err)
	}
}
