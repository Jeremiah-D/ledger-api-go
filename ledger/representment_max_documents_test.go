package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Dispute representment evidence-package cap runtime revision tests
// (LG-357). They reuse mustAuditLog from ledger_audit_test.go,
// seedRepresentmentLedger from ledger_representment_test.go, and
// ReadAuditLog from the audit log reader.

// seedAuditedRepresentmentLedger builds the seedRepresentmentLedger
// fixture (one open dispute d1 on transfer t1) on a ledger with an
// audit log attached, for the tests that read revision events back.
func seedAuditedRepresentmentLedger(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{
		ID:           "fund-payer",
		DebitAccount: "payer",
		CreditAccount: "equity",
		AmountCents:  20000,
		Currency:     "USD",
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID:          "t1",
		From:        "payer",
		To:          "payee",
		AmountCents: 10000,
		Currency:    "USD",
	}); err != nil {
		t.Fatalf("seed PostTransfer: %v", err)
	}
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	}); err != nil {
		t.Fatalf("seed RaiseDispute: %v", err)
	}
	return l, al
}

// retunedRepresentmentMaxDocumentsEvents closes the audit log (which
// drains the async writer) and returns every
// representment_max_documents_retuned event recorded in it. The
// ledger must not be used after this call.
func retunedRepresentmentMaxDocumentsEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "representment_max_documents_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// docHashes builds n distinct document hashes for a package of n.
func docHashes(n int) []string {
	hashes := make([]string, n)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("sha256:doc-%03d", i)
	}
	return hashes
}

func TestSetRepresentmentMaxDocumentsInvalidRejectedAndKeepsLive(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	if got := l.RepresentmentMaxDocuments(); got != 32 {
		t.Fatalf("default cap = %d, want 32", got)
	}
	for _, n := range []int{0, -1, -100} {
		prev, err := l.SetRepresentmentMaxDocuments(n)
		if !errors.Is(err, ErrInvalidRepresentmentMaxDocuments) {
			t.Fatalf("SetRepresentmentMaxDocuments(%d): err = %v, want ErrInvalidRepresentmentMaxDocuments", n, err)
		}
		if prev != 32 {
			t.Fatalf("SetRepresentmentMaxDocuments(%d) returned %d, want the live 32", n, prev)
		}
		if got := l.RepresentmentMaxDocuments(); got != 32 {
			t.Fatalf("cap = %d after rejected %d, want still 32", got, n)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetRepresentmentMaxDocumentsCtx(context.Background(), 0); !errors.Is(err, ErrInvalidRepresentmentMaxDocuments) || prev != 32 {
		t.Fatalf("SetRepresentmentMaxDocumentsCtx(0) = (%d, %v), want (32, ErrInvalidRepresentmentMaxDocuments)", prev, err)
	}
	// Zero is rejected here because a zero cap would make any
	// representment carrying a document fail — a runtime revision
	// never silently disables evidence submission.
	if events := retunedRepresentmentMaxDocumentsEvents(t, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetRepresentmentMaxDocumentsSameValueNoOpNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	prev, err := l.SetRepresentmentMaxDocuments(32)
	if err != nil {
		t.Fatalf("same-value SetRepresentmentMaxDocuments: %v", err)
	}
	if prev != 32 {
		t.Fatalf("same-value revision returned %d, want the live 32", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := l.SetRepresentmentMaxDocuments(64); err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments(64): %v", err)
	}
	if prev, err := l.SetRepresentmentMaxDocuments(64); err != nil || prev != 64 {
		t.Fatalf("same-value SetRepresentmentMaxDocuments(64) = (%d, %v), want (64, nil)", prev, err)
	}
	if events := retunedRepresentmentMaxDocumentsEvents(t, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetRepresentmentMaxDocumentsReturnsPreviousValue(t *testing.T) {
	l := New()
	prev, err := l.SetRepresentmentMaxDocuments(64)
	if err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments: %v", err)
	}
	if prev != 32 {
		t.Fatalf("returned previous cap = %d, want 32", prev)
	}
	if got := l.RepresentmentMaxDocuments(); got != 64 {
		t.Fatalf("cap = %d after the revision, want 64", got)
	}
}

func TestSetRepresentmentMaxDocumentsWidenedAllows33Hashes(t *testing.T) {
	l := seedRepresentmentLedger(t)
	// 33 hashes exceed the default 32: widen the cap first.
	if _, _, err := l.SubmitRepresentment("d1", docHashes(33), "complex evidence chain"); !errors.Is(err, ErrInvalidRepresentmentDocument) {
		t.Fatalf("33 hashes at default cap = %v, want ErrInvalidRepresentmentDocument", err)
	}
	if _, err := l.SetRepresentmentMaxDocuments(64); err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments(64): %v", err)
	}
	r, dup, err := l.SubmitRepresentment("d1", docHashes(33), "complex evidence chain")
	if err != nil {
		t.Fatalf("33 hashes after widening: %v", err)
	}
	if dup {
		t.Error("first submit reports duplicate = true")
	}
	if len(r.DocumentHashes) != 33 {
		t.Fatalf("document_hashes = %d, want 33", len(r.DocumentHashes))
	}
}

func TestSetRepresentmentMaxDocumentsTightenedRejectsOverLimit(t *testing.T) {
	l := seedRepresentmentLedger(t)
	if _, err := l.SetRepresentmentMaxDocuments(2); err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments(2): %v", err)
	}
	if _, _, err := l.SubmitRepresentment("d1", docHashes(3), "s"); !errors.Is(err, ErrInvalidRepresentmentDocument) {
		t.Fatalf("3 hashes at cap 2 = %v, want ErrInvalidRepresentmentDocument", err)
	}
	// The rejected submission leaves the live cap untouched, and a
	// package within the new cap still files.
	if got := l.RepresentmentMaxDocuments(); got != 2 {
		t.Fatalf("cap = %d after rejected submission, want still 2", got)
	}
	if _, _, err := l.SubmitRepresentment("d1", docHashes(2), "s"); err != nil {
		t.Fatalf("2 hashes at cap 2: %v", err)
	}
}

func TestSetRepresentmentMaxDocumentsAlreadySubmittedUnaffected(t *testing.T) {
	l := seedRepresentmentLedger(t)
	hashes := docHashes(2)
	if _, _, err := l.SubmitRepresentment("d1", hashes, "authorized"); err != nil {
		t.Fatalf("first SubmitRepresentment: %v", err)
	}
	// Tighten below the filed package size: the stored package
	// stays valid — the revision governs only future submissions.
	if _, err := l.SetRepresentmentMaxDocuments(1); err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments(1): %v", err)
	}
	got, ok := l.GetRepresentment("d1")
	if !ok || len(got.DocumentHashes) != 2 {
		t.Fatalf("stored representment = %+v, want the 2-hash package intact after tightening", got)
	}
	// Replay of the same evidence still returns the original, and
	// adjudication still works on the stored package as filed.
	if _, dup, err := l.SubmitRepresentment("d1", hashes, "authorized"); err != nil || !dup {
		t.Fatalf("replay after tightening = (dup=%v, err=%v), want (true, nil)", dup, err)
	}
	resolved, err := l.AdjudicateRepresentment("d1", RepresentmentDecisionReverseClaim, "arbiter-1")
	if err != nil {
		t.Fatalf("AdjudicateRepresentment after tightening: %v", err)
	}
	if resolved.Status != DisputeStatusResolved {
		t.Fatalf("resolved status = %q, want resolved", resolved.Status)
	}
	r, _ := l.GetRepresentment("d1")
	if !r.Adjudicated || r.Decision != RepresentmentDecisionReverseClaim {
		t.Fatalf("adjudicated representment = %+v, want reverse decision recorded", r)
	}
}

func TestSetRepresentmentMaxDocumentsConcurrentRevisionAndSubmitRaceFree(t *testing.T) {
	l := seedRepresentmentLedger(t)

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while
	// submissions run underneath: -race must stay silent, and the
	// cap must stay a valid live value throughout. Submissions to
	// the same dispute may succeed or fail depending on the live
	// cap at the time — either outcome is safe under the lock.
	caps := []int{1, 2, 32, 64, 100}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_, _ = l.SetRepresentmentMaxDocuments(caps[(g+i)%len(caps)])
				_ = l.RepresentmentMaxDocuments()
			}
		}(g)
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_, _, _ = l.SubmitRepresentment("d1", docHashes(2), "s")
			}
		}()
	}
	wg.Wait()
	got := l.RepresentmentMaxDocuments()
	valid := false
	for _, c := range caps {
		if got == c {
			valid = true
		}
	}
	if !valid {
		t.Fatalf("cap = %d after concurrent revisions, want one of %v", got, caps)
	}
}

func TestSetRepresentmentMaxDocumentsCtxActorAttribution(t *testing.T) {
	l, al := seedAuditedRepresentmentLedger(t)
	defer func() { _ = al.Close() }()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := l.SetRepresentmentMaxDocumentsCtx(ctx, 64); err != nil {
		t.Fatalf("SetRepresentmentMaxDocumentsCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := l.SetRepresentmentMaxDocuments(16); err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments: %v", err)
	}
	events := retunedRepresentmentMaxDocumentsEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_max_documents"]; got != float64(32) {
		t.Fatalf("first previous_max_documents = %v, want 32", got)
	}
	if got := events[0].Details["current_max_documents"]; got != float64(64) {
		t.Fatalf("first current_max_documents = %v, want 64", got)
	}
	if events[1].Actor != "SetRepresentmentMaxDocuments" {
		t.Fatalf("second event Actor = %q, want SetRepresentmentMaxDocuments", events[1].Actor)
	}
	if got := events[1].Details["previous_max_documents"]; got != float64(64) {
		t.Fatalf("second previous_max_documents = %v, want 64", got)
	}
	if got := events[1].Details["current_max_documents"]; got != float64(16) {
		t.Fatalf("second current_max_documents = %v, want 16", got)
	}
}

func TestSetRepresentmentMaxDocumentsAuditPreviousCurrentExact(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	prev, err := l.SetRepresentmentMaxDocuments(64)
	if err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments: %v", err)
	}
	if prev != 32 {
		t.Fatalf("returned previous cap = %d, want 32", prev)
	}
	events := retunedRepresentmentMaxDocumentsEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetRepresentmentMaxDocuments" {
		t.Errorf("Actor = %q, want SetRepresentmentMaxDocuments", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_max_documents"]; got != float64(32) {
		t.Errorf("previous_max_documents = %v, want 32", got)
	}
	if got := ev.Details["current_max_documents"]; got != float64(64) {
		t.Errorf("current_max_documents = %v, want 64", got)
	}
	// Only the two integer fields ride in the event: no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current max documents", ev.Details)
	}
}

func TestSetRepresentmentMaxDocumentsGetterFollowsLiveValue(t *testing.T) {
	l := New()
	if got := l.RepresentmentMaxDocuments(); got != 32 {
		t.Fatalf("cap = %d at start, want 32", got)
	}
	if prev, err := l.SetRepresentmentMaxDocuments(64); err != nil || prev != 32 {
		t.Fatalf("SetRepresentmentMaxDocuments(64) = (%d, %v), want (32, nil)", prev, err)
	}
	if got := l.RepresentmentMaxDocuments(); got != 64 {
		t.Fatalf("cap = %d after widening, want 64", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := l.SetRepresentmentMaxDocuments(0); !errors.Is(err, ErrInvalidRepresentmentMaxDocuments) {
		t.Fatalf("SetRepresentmentMaxDocuments(0): err = %v, want ErrInvalidRepresentmentMaxDocuments", err)
	}
	if got := l.RepresentmentMaxDocuments(); got != 64 {
		t.Fatalf("cap = %d after rejected revision, want still 64", got)
	}
}

func TestSetRepresentmentMaxDocumentsSnapshotRestoresDefault(t *testing.T) {
	l := New()
	if _, err := l.SetRepresentmentMaxDocuments(64); err != nil {
		t.Fatalf("SetRepresentmentMaxDocuments(64): %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// Snapshots do not carry the cap: a restore starts again at the
	// default 32, while the revision event stays in the audit trail
	// for traceability.
	if got := restored.RepresentmentMaxDocuments(); got != 32 {
		t.Fatalf("restored cap = %d, want the default 32", got)
	}
}
