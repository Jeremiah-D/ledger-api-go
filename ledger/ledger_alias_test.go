package ledger

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSetAliasBasic(t *testing.T) {
	l := New()
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	if got, ok := l.AliasOf("cash"); !ok || got != "ops_cash" {
		t.Errorf("AliasOf(cash) = %q (%v), want ops_cash (true)", got, ok)
	}
	if got, ok := l.AliasAccount("ops_cash"); !ok || got != "cash" {
		t.Errorf("AliasAccount(ops_cash) = %q (%v), want cash (true)", got, ok)
	}
	if _, ok := l.AliasOf("equity"); ok {
		t.Error("AliasOf(equity) = true, want false")
	}
	if _, ok := l.AliasAccount("nope"); ok {
		t.Error("AliasAccount(nope) = true, want false")
	}
}

func TestResolveAccount(t *testing.T) {
	l := New()
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	// A registered alias resolves to the account it names.
	if got, alias := l.ResolveAccount("ops_cash"); got != "cash" || alias != "ops_cash" {
		t.Errorf("ResolveAccount(ops_cash) = (%q, %q), want (cash, ops_cash)", got, alias)
	}
	// The canonical ID resolves to itself and still reports the alias.
	if got, alias := l.ResolveAccount("cash"); got != "cash" || alias != "ops_cash" {
		t.Errorf("ResolveAccount(cash) = (%q, %q), want (cash, ops_cash)", got, alias)
	}
	// Anything else is a usable account ID with no alias.
	if got, alias := l.ResolveAccount("equity"); got != "equity" || alias != "" {
		t.Errorf("ResolveAccount(equity) = (%q, %q), want (equity, \"\")", got, alias)
	}
}

func TestSetAliasConflict(t *testing.T) {
	l := New()
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	// The same alias on a different account is rejected.
	if err := l.SetAlias("equity", "ops_cash"); !errors.Is(err, ErrAliasConflict) {
		t.Fatalf("conflict err = %v, want ErrAliasConflict", err)
	}
	// The failed bind changed nothing: the alias still names cash, and
	// equity is still unaliased.
	if got, _ := l.AliasAccount("ops_cash"); got != "cash" {
		t.Errorf("AliasAccount(ops_cash) = %q, want cash", got)
	}
	if _, ok := l.AliasOf("equity"); ok {
		t.Error("AliasOf(equity) = true after failed bind, want false")
	}
}

func TestSetAliasRebind(t *testing.T) {
	l := New()
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAlias("cash", "petty_cash"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if got, ok := l.AliasOf("cash"); !ok || got != "petty_cash" {
		t.Errorf("AliasOf(cash) = %q (%v), want petty_cash (true)", got, ok)
	}
	// The old alias is unbound atomically with the rebind.
	if _, ok := l.AliasAccount("ops_cash"); ok {
		t.Error("AliasAccount(ops_cash) still bound after rebind")
	}
	if got, ok := l.AliasAccount("petty_cash"); !ok || got != "cash" {
		t.Errorf("AliasAccount(petty_cash) = %q (%v), want cash (true)", got, ok)
	}
}

func TestSetAliasIdempotent(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	// Repeating the identical bind is a silent no-op: no error, and no
	// second audit event.
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	n := 0
	for _, e := range events {
		if e.Op == "alias_set" || e.Op == "alias_changed" || e.Op == "alias_removed" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("alias audit events = %d, want 1 (idempotent replay must stay silent)", n)
	}
}

func TestClearAlias(t *testing.T) {
	l := New()
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	if err := l.ClearAlias("cash"); err != nil {
		t.Fatalf("ClearAlias: %v", err)
	}
	if _, ok := l.AliasOf("cash"); ok {
		t.Error("AliasOf(cash) = true after clear, want false")
	}
	if _, ok := l.AliasAccount("ops_cash"); ok {
		t.Error("AliasAccount(ops_cash) = true after clear, want false")
	}
	// SetAlias with the empty alias is the same clear.
	if err := l.SetAlias("equity", "eq"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAlias("equity", ""); err != nil {
		t.Fatalf("SetAlias(equity, \"\"): %v", err)
	}
	if _, ok := l.AliasOf("equity"); ok {
		t.Error("AliasOf(equity) = true after empty-alias clear, want false")
	}
	// Clearing an unaliased account is a silent no-op.
	if err := l.ClearAlias("nobody"); err != nil {
		t.Errorf("ClearAlias(nobody) = %v, want nil", err)
	}
}

func TestSetAliasValidation(t *testing.T) {
	l := New()
	if err := l.SetAlias("", "ops_cash"); !errors.Is(err, ErrEmptyAliasAccount) {
		t.Errorf("empty account err = %v, want ErrEmptyAliasAccount", err)
	}
	for _, alias := range []string{"has space", "bad/key", "semi;colon", "ütf8", strings.Repeat("a", 65)} {
		if err := l.SetAlias("cash", alias); !errors.Is(err, ErrInvalidAlias) {
			t.Errorf("alias %q err = %v, want ErrInvalidAlias", alias, err)
		}
	}
	// Rejected binds install nothing.
	if _, ok := l.AliasOf("cash"); ok {
		t.Error("AliasOf(cash) = true after rejected binds, want false")
	}
	// The documented charset is accepted, including a 64-char alias.
	if err := l.SetAlias("cash", "aZ09._-"); err != nil {
		t.Errorf("charset alias: %v", err)
	}
	if err := l.SetAlias("equity", strings.Repeat("b", 64)); err != nil {
		t.Errorf("64-char alias: %v", err)
	}
}

func TestSetAliasSelfBinding(t *testing.T) {
	l := New()
	// Binding an alias identical to the account ID is allowed and
	// documented: it resolves exactly like the plain ID.
	if err := l.SetAlias("cash", "cash"); err != nil {
		t.Fatalf("self bind: %v", err)
	}
	if got, alias := l.ResolveAccount("cash"); got != "cash" || alias != "cash" {
		t.Errorf("ResolveAccount(cash) = (%q, %q), want (cash, cash)", got, alias)
	}
	// It still occupies the alias namespace: another account cannot
	// take it.
	if err := l.SetAlias("equity", "cash"); !errors.Is(err, ErrAliasConflict) {
		t.Errorf("conflict on self-bound alias err = %v, want ErrAliasConflict", err)
	}
}

func TestSetAliasAuditEvents(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAlias("cash", "petty_cash"); err != nil {
		t.Fatal(err)
	}
	if err := l.ClearAlias("cash"); err != nil {
		t.Fatal(err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	type want struct {
		op       string
		account  string
		oldAlias string
		newAlias string
	}
	wants := []want{
		{op: "alias_set", account: "cash", newAlias: "ops_cash"},
		{op: "alias_changed", account: "cash", oldAlias: "ops_cash", newAlias: "petty_cash"},
		{op: "alias_removed", account: "cash", oldAlias: "petty_cash"},
	}
	got := 0
	for _, e := range events {
		if e.Op != "alias_set" && e.Op != "alias_changed" && e.Op != "alias_removed" {
			continue
		}
		if got >= len(wants) {
			t.Fatalf("extra alias event: %+v", e)
		}
		w := wants[got]
		got++
		if e.Op != w.op {
			t.Errorf("event %d op = %q, want %q", got, e.Op, w.op)
		}
		if e.TraceID != w.account {
			t.Errorf("event %d trace_id = %q, want %q", got, e.TraceID, w.account)
		}
		if len(e.Accounts) != 1 || e.Accounts[0] != AccountID(w.account) {
			t.Errorf("event %d accounts = %v, want [%s]", got, e.Accounts, w.account)
		}
		if w.oldAlias != "" && e.Details["old_alias"] != w.oldAlias {
			t.Errorf("event %d old_alias = %v, want %q", got, e.Details["old_alias"], w.oldAlias)
		}
		if w.newAlias != "" && e.Details["new_alias"] != w.newAlias {
			t.Errorf("event %d new_alias = %v, want %q", got, e.Details["new_alias"], w.newAlias)
		}
		// Alias changes are structural: the journal version does not move.
		if e.VersionBefore != e.VersionAfter {
			t.Errorf("event %d version moved %d -> %d, want unchanged", got, e.VersionBefore, e.VersionAfter)
		}
	}
	if got != len(wants) {
		t.Errorf("alias events = %d, want %d", got, len(wants))
	}
}

func TestSetAliasCtxActor(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-bot")
	if err := l.SetAliasCtx(ctx, "cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	found := false
	for _, e := range events {
		if e.Op == "alias_set" {
			found = true
			if e.Actor != "ops-bot" {
				t.Errorf("alias_set actor = %q, want ops-bot", e.Actor)
			}
		}
	}
	if !found {
		t.Error("no alias_set event found")
	}
}

func TestAliasConcurrency(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	// Writers bind distinct aliases; readers resolve them concurrently.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			account := AccountID(strings.Repeat("a", i+1) + "cct")
			alias := strings.Repeat("b", i+1) + "lias"
			if err := l.SetAlias(account, alias); err != nil {
				t.Errorf("SetAlias(%s): %v", account, err)
				return
			}
			if got, ok := l.AliasOf(account); !ok || got != alias {
				t.Errorf("AliasOf(%s) = %q (%v), want %q", account, got, ok, alias)
			}
			if got, _ := l.ResolveAccount(alias); got != account {
				t.Errorf("ResolveAccount(%s) = %q, want %q", alias, got, account)
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.ResolveAccount("whatever")
				l.AliasOf("cash")
				l.AliasAccount("nope")
			}
		}()
	}
	wg.Wait()
}

func TestAliasSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAlias("equity", "eq"); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	restored, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got, ok := restored.AliasOf("cash"); !ok || got != "ops_cash" {
		t.Errorf("restored AliasOf(cash) = %q (%v), want ops_cash (true)", got, ok)
	}
	if got, ok := restored.AliasOf("equity"); !ok || got != "eq" {
		t.Errorf("restored AliasOf(equity) = %q (%v), want eq (true)", got, ok)
	}
	if got, alias := restored.ResolveAccount("ops_cash"); got != "cash" || alias != "ops_cash" {
		t.Errorf("restored ResolveAccount(ops_cash) = (%q, %q), want (cash, ops_cash)", got, alias)
	}
	// A snapshot whose alias rows conflict fails the import instead of
	// installing a mapping SetAlias could never have created.
	var tampered strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, `"account_aliases"`) {
			line = strings.Replace(line, `"alias":"eq"`, `"alias":"ops_cash"`, 1)
		}
		tampered.WriteString(line + "\n")
	}
	if _, err := ImportSnapshot(strings.NewReader(tampered.String())); err == nil {
		t.Error("ImportSnapshot with conflicting alias rows succeeded, want failure")
	}
}

func TestTrialBalanceEchoesAlias(t *testing.T) {
	l := New()
	tb := l.TrialBalance("cash")
	if tb.Alias != "" {
		t.Errorf("unaliased trial balance alias = %q, want empty", tb.Alias)
	}
	if err := l.SetAlias("cash", "ops_cash"); err != nil {
		t.Fatal(err)
	}
	tb = l.TrialBalance("cash")
	if tb.Alias != "ops_cash" {
		t.Errorf("trial balance alias = %q, want ops_cash", tb.Alias)
	}
}
