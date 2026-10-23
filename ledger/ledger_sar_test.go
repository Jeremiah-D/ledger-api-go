package ledger

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// sarTestLedger builds a ledger with a USD 10,000 large-tx threshold.
func sarTestLedger(t *testing.T, opts ...Option) *Ledger {
	t.Helper()
	all := append([]Option{WithLargeTxThresholds(map[string]int64{"USD": 1000000})}, opts...)
	return New(all...)
}

func sarPost(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  debit,
		CreditAccount: credit,
		AmountCents:   cents,
		Currency:      "USD",
		CreatedAt:     at,
	}); err != nil {
		t.Fatalf("post %s: %v", id, err)
	}
}

func sarWindow() (since, until time.Time) {
	// until sits an hour in the future so entries posted "now" by the
	// tests land inside the window.
	until = time.Now().Add(time.Hour)
	since = until.Add(-25 * time.Hour)
	return since, until
}

func hasReason(rep *SARReport, code SARReasonCode) bool {
	for _, c := range rep.ReasonCodes {
		if c == code {
			return true
		}
	}
	return false
}

func hasSighting(rep *SARReport, code SARReasonCode, source string) bool {
	for _, s := range rep.Sightings {
		if s.Reason == code && (source == "" || s.Source == source) {
			return true
		}
	}
	return false
}

func TestDraftSARStructuring(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	now := time.Now()
	// Three 8,000.00 inflows: each >= 0.75 * 10,000 threshold floor and
	// below the threshold — the classic placement cluster.
	for i := 0; i < 3; i++ {
		sarPost(t, l, fmt.Sprintf("struct-%d", i), "acct", "equity", 800000, now.Add(-time.Duration(i)*time.Minute))
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if rep.Status != SARDraft {
		t.Errorf("status = %q, want draft", rep.Status)
	}
	if !hasReason(rep, SARReasonStructuring) {
		t.Errorf("reason codes %v: missing STRUCTURING", rep.ReasonCodes)
	}
	if !hasSighting(rep, SARReasonStructuring, "detector") {
		t.Errorf("sightings: missing STRUCTURING detector sighting")
	}
	if len(rep.Transactions) != 3 {
		t.Errorf("cited transactions = %d, want 3", len(rep.Transactions))
	}
	if rep.ID == "" || !strings.HasPrefix(rep.ID, "SAR-") {
		t.Errorf("report ID = %q, want SAR-NNNN", rep.ID)
	}
	if rep.Narrative == "" {
		t.Errorf("narrative is empty")
	}
	// Drafting books nothing and bumps no version.
	if got := l.Balance("acct"); got != 2400000 {
		t.Errorf("balance = %d, want 2400000 (draft must not move money)", got)
	}
}

func TestDraftSARStructuringBelowMinCount(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	now := time.Now()
	// Only two sub-threshold amounts: below the min-count of 3. The day's
	// aggregate still crosses the large-tx threshold, so a draft exists —
	// but it must not carry STRUCTURING.
	for i := 0; i < 2; i++ {
		sarPost(t, l, fmt.Sprintf("struct2-%d", i), "acct", "equity", 800000, now.Add(-time.Duration(i)*time.Minute))
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if hasReason(rep, SARReasonStructuring) {
		t.Errorf("reason codes %v: unexpected STRUCTURING below min-count", rep.ReasonCodes)
	}
	if !hasReason(rep, SARReasonLargeTxFlow) {
		t.Errorf("reason codes %v: want LARGE_TX_FLOW from the day aggregate", rep.ReasonCodes)
	}
}

func TestDraftSARLargeTxFlow(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	// Exactly at the threshold: inflow crosses.
	sarPost(t, l, "big-1", "acct", "equity", 1000000, time.Now())
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if !hasReason(rep, SARReasonLargeTxFlow) {
		t.Errorf("reason codes %v: missing LARGE_TX_FLOW", rep.ReasonCodes)
	}
	if !hasSighting(rep, SARReasonLargeTxFlow, "ctr") {
		t.Errorf("sightings: missing LARGE_TX_FLOW ctr sighting")
	}
	// No structuring (at-threshold is not below the threshold) and no
	// rapid movement (no outflow).
	if hasReason(rep, SARReasonStructuring) {
		t.Errorf("reason codes %v: unexpected STRUCTURING", rep.ReasonCodes)
	}
	if hasReason(rep, SARReasonRapidMovement) {
		t.Errorf("reason codes %v: unexpected RAPID_MOVEMENT", rep.ReasonCodes)
	}
}

func TestDraftSARVelocityAnomaly(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	if err := l.SetVelocityLimit("acct", "USD", 24*time.Hour, 10, 0); err != nil {
		t.Fatalf("SetVelocityLimit: %v", err)
	}
	now := time.Now()
	// 8 postings against a 10-posting limit: usage sits in the 0.8 warn band.
	for i := 0; i < 8; i++ {
		sarPost(t, l, fmt.Sprintf("vel-%d", i), "acct", "equity", 100, now.Add(-time.Duration(i)*time.Minute))
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if !hasReason(rep, SARReasonVelocityAnomaly) {
		t.Errorf("reason codes %v: missing VELOCITY_ANOMALY", rep.ReasonCodes)
	}
	if !hasSighting(rep, SARReasonVelocityAnomaly, "velocity") {
		t.Errorf("sightings: missing VELOCITY_ANOMALY velocity sighting")
	}
}

func TestDraftSARVelocityBelowWarnBand(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	if err := l.SetVelocityLimit("acct", "USD", 24*time.Hour, 100, 0); err != nil {
		t.Fatalf("SetVelocityLimit: %v", err)
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		sarPost(t, l, fmt.Sprintf("velb-%d", i), "acct", "equity", 100, now.Add(-time.Duration(i)*time.Minute))
	}
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR err = %v, want ErrNoSuspiciousActivity", err)
	}
}

func TestDraftSARDenylistMatch(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	l.AddDeniedAccount("acct")
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if !hasReason(rep, SARReasonDenylistMatch) {
		t.Errorf("reason codes %v: missing DENYLIST_MATCH", rep.ReasonCodes)
	}
	if !hasSighting(rep, SARReasonDenylistMatch, "denylist") {
		t.Errorf("sightings: missing DENYLIST_MATCH denylist sighting")
	}
}

func TestDraftSARRapidMovement(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	now := time.Now()
	// 5,000 in, then 4,500 out within the 4h window: 0.9 >= 0.8.
	sarPost(t, l, "rm-in", "acct", "equity", 500000, now.Add(-2*time.Hour))
	sarPost(t, l, "rm-out", "equity", "acct", 450000, now.Add(-time.Hour))
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if !hasReason(rep, SARReasonRapidMovement) {
		t.Errorf("reason codes %v: missing RAPID_MOVEMENT", rep.ReasonCodes)
	}
	if !hasSighting(rep, SARReasonRapidMovement, "detector") {
		t.Errorf("sightings: missing RAPID_MOVEMENT detector sighting")
	}
	// Below-threshold legs: no large-tx, no structuring.
	if hasReason(rep, SARReasonLargeTxFlow) {
		t.Errorf("reason codes %v: unexpected LARGE_TX_FLOW", rep.ReasonCodes)
	}
}

func TestDraftSARRapidMovementBelowRatio(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	now := time.Now()
	// 5,000 in, only 1,000 out: 0.2 < 0.8 — no sighting.
	sarPost(t, l, "rmb-in", "acct", "equity", 500000, now.Add(-2*time.Hour))
	sarPost(t, l, "rmb-out", "equity", "acct", 100000, now.Add(-time.Hour))
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR err = %v, want ErrNoSuspiciousActivity", err)
	}
}

func TestDraftSARNoSignal(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	sarPost(t, l, "plain-1", "acct", "equity", 5000, time.Now())
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR err = %v, want ErrNoSuspiciousActivity", err)
	}
}

func TestDraftSARValidation(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	if _, err := l.DraftSAR("", since, until); !errors.Is(err, ErrEmptySARSubject) {
		t.Errorf("empty subject err = %v, want ErrEmptySARSubject", err)
	}
	if _, err := l.DraftSAR("acct", until, since); !errors.Is(err, ErrInvalidSARWindow) {
		t.Errorf("inverted window err = %v, want ErrInvalidSARWindow", err)
	}
	if _, err := l.DraftSAR("acct", since, since.Add(32*24*time.Hour)); !errors.Is(err, ErrInvalidSARWindow) {
		t.Errorf("over-long window err = %v, want ErrInvalidSARWindow", err)
	}
	if _, err := l.DraftSAR("acct", time.Time{}, until); !errors.Is(err, ErrInvalidSARWindow) {
		t.Errorf("zero since err = %v, want ErrInvalidSARWindow", err)
	}
}

func TestDraftSARKycSubject(t *testing.T) {
	l := sarTestLedger(t, WithSARReportingEntity("Test Bank N.A."))
	since, until := sarWindow()
	if err := l.SetKyc("acct", KycProfile{Account: "acct", Status: "verified", RiskTier: "high"}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	l.AddDeniedAccount("acct")
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if rep.Subject.KycStatus != "verified" || rep.Subject.RiskTier != "high" {
		t.Errorf("subject = %+v, want KYC status/risk tier", rep.Subject)
	}
	if rep.ReportingEntity != "Test Bank N.A." {
		t.Errorf("reporting entity = %q", rep.ReportingEntity)
	}
}

func TestFileSARLifecycle(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	l := sarTestLedger(t, WithAuditLog(al))
	since, until := sarWindow()
	sarPost(t, l, "file-1", "acct", "equity", 1000000, time.Now())

	draft, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if draft.FiledAt != nil || draft.ContentHash != "" {
		t.Errorf("draft carries filing fields: %+v", draft)
	}

	filed, err := l.FileSAR(draft.ID)
	if err != nil {
		t.Fatalf("FileSAR: %v", err)
	}
	if filed.Status != SARFiled {
		t.Errorf("status = %q, want filed", filed.Status)
	}
	if filed.FiledAt == nil {
		t.Errorf("filed report has no FiledAt")
	}
	if filed.ContentHash == "" {
		t.Errorf("filed report has no content hash")
	}

	// Filed reports are immutable: refiling is rejected.
	if _, err := l.FileSAR(draft.ID); !errors.Is(err, ErrSARAlreadyFiled) {
		t.Errorf("refile err = %v, want ErrSARAlreadyFiled", err)
	}
	if _, err := l.FileSAR("SAR-9999"); !errors.Is(err, ErrSARNotFound) {
		t.Errorf("unknown id err = %v, want ErrSARNotFound", err)
	}

	// GetSAR returns copies: mutating the returned value must not touch
	// the stored report.
	got, ok := l.GetSAR(draft.ID)
	if !ok {
		t.Fatalf("GetSAR: not found")
	}
	got.Narrative = "tampered"
	again, _ := l.GetSAR(draft.ID)
	if again.Narrative == "tampered" {
		t.Errorf("GetSAR returned a live reference: stored report mutated")
	}

	// The filing is sealed into the audit chain.
	day := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(al.Dir(), day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var found *AuditEvent
	for i := range events {
		if events[i].Op == "sar_filed" {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no sar_filed audit event")
	}
	if found.Details["content_hash"] != filed.ContentHash {
		t.Errorf("audit content_hash = %v, want %s", found.Details["content_hash"], filed.ContentHash)
	}
	if found.Details["sar_id"] != draft.ID {
		t.Errorf("audit sar_id = %v, want %s", found.Details["sar_id"], draft.ID)
	}
	if _, err := VerifyAuditLog(al.Dir()); err != nil {
		t.Errorf("VerifyAuditLog: %v", err)
	}

	drafts, filedN := l.SARStats()
	if drafts != 0 || filedN != 1 {
		t.Errorf("SARStats = (%d, %d), want (0, 1)", drafts, filedN)
	}
}

func TestListSARsOrdering(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	now := time.Now()
	sarPost(t, l, "ord-1", "a1", "equity", 1000000, now)
	sarPost(t, l, "ord-2", "a2", "equity", 1000000, now)
	r1, err := l.DraftSAR("a1", since, until)
	if err != nil {
		t.Fatalf("draft a1: %v", err)
	}
	r2, err := l.DraftSAR("a2", since, until)
	if err != nil {
		t.Fatalf("draft a2: %v", err)
	}
	if r1.ID == r2.ID {
		t.Fatalf("duplicate report IDs: %q", r1.ID)
	}
	list := l.ListSARs()
	if len(list) != 2 {
		t.Fatalf("ListSARs = %d, want 2", len(list))
	}
	if list[0].ID != r1.ID || list[1].ID != r2.ID {
		t.Errorf("ListSARs order = [%s %s], want [%s %s]", list[0].ID, list[1].ID, r1.ID, r2.ID)
	}
	drafts, filed := l.SARStats()
	if drafts != 2 || filed != 0 {
		t.Errorf("SARStats = (%d, %d), want (2, 0)", drafts, filed)
	}
}

func TestSAROptionPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  func() Option
	}{
		{"structuring ratio zero", func() Option { return WithSARStructuring(0, 3) }},
		{"structuring ratio one", func() Option { return WithSARStructuring(1, 3) }},
		{"structuring minCount one", func() Option { return WithSARStructuring(0.5, 1) }},
		{"velocity warn ratio zero", func() Option { return WithSARVelocityWarnRatio(0) }},
		{"rapid window zero", func() Option { return WithSARRapidMovement(0, 0.5) }},
		{"rapid ratio zero", func() Option { return WithSARRapidMovement(time.Hour, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("expected panic")
				}
			}()
			tc.opt()
		})
	}
}

func TestDraftSARConcurrent(t *testing.T) {
	l := sarTestLedger(t)
	since, until := sarWindow()
	now := time.Now()
	for i := 0; i < 4; i++ {
		sarPost(t, l, fmt.Sprintf("conc-%d", i), "acct", "equity", 1000000, now)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.DraftSAR("acct", since, until)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if n := len(l.ListSARs()); n != 8 {
		t.Errorf("ListSARs = %d, want 8", n)
	}
	// IDs are unique even under concurrency.
	seen := make(map[string]bool)
	for _, r := range l.ListSARs() {
		if seen[r.ID] {
			t.Errorf("duplicate report ID %q", r.ID)
		}
		seen[r.ID] = true
	}
}
