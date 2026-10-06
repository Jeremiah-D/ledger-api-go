package ledger

import "testing"

func TestSnapshotVersionedBalance(t *testing.T) {
	l := New()

	// Fresh ledger: unknown account snapshots to zero balance, version zero.
	if bal, ver := l.Snapshot("cash"); bal != 0 || ver != 0 {
		t.Fatalf("Snapshot on empty ledger = (%d, %d), want (0, 0)", bal, ver)
	}

	// Each successful Post bumps the version; balance follows bookings.
	for i, want := range []struct {
		balance int64
		version uint64
	}{
		{1000, 1},
		{2000, 2},
	} {
		e := validEntry()
		e.ID = "e-snap-" + string(rune('a'+i))
		e.IdempotencyKey = "key-snap-" + string(rune('a'+i))
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post %d = dup=%v err=%v", i, dup, err)
		}
		bal, ver := l.Snapshot("cash")
		if bal != want.balance || ver != want.version {
			t.Fatalf("Snapshot(cash) after post %d = (%d, %d), want (%d, %d)",
				i, bal, ver, want.balance, want.version)
		}
	}

	// The other side of the booking moves in lockstep with the same version.
	if bal, ver := l.Snapshot("equity"); bal != -2000 || ver != 2 {
		t.Fatalf("Snapshot(equity) = (%d, %d), want (-2000, 2)", bal, ver)
	}

	// An idempotent replay books nothing, so the version must not move.
	retry := validEntry()
	retry.ID = "e-snap-retry"
	retry.IdempotencyKey = "key-snap-a"
	if _, dup, err := l.Post(retry); err != nil || !dup {
		t.Fatalf("replay Post = dup=%v err=%v, want dup=true", dup, err)
	}
	if bal, ver := l.Snapshot("cash"); bal != 2000 || ver != 2 {
		t.Fatalf("Snapshot(cash) after replay = (%d, %d), want (2000, 2)", bal, ver)
	}

	// A rejected post leaves both balance and version untouched.
	bad := validEntry()
	bad.ID = "e-snap-bad"
	bad.IdempotencyKey = "key-snap-bad"
	bad.AmountCents = 0
	if _, _, err := l.Post(bad); err == nil {
		t.Fatalf("expected rejection of zero-amount entry")
	}
	if bal, ver := l.Snapshot("cash"); bal != 2000 || ver != 2 {
		t.Fatalf("Snapshot(cash) after rejected post = (%d, %d), want (2000, 2)", bal, ver)
	}
}
