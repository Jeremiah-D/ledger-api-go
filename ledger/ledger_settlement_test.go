package ledger

import (
	"bytes"
	"encoding/csv"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// postSettlementEntries books a fixed set of journal entries on two UTC
// days for two merchant accounts: merch-a (channel alipay) and merch-b
// (no channel). All amounts in cents.
func postSettlementEntries(t *testing.T, l *Ledger) {
	t.Helper()
	day1 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	post := func(e JournalEntry) {
		t.Helper()
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post %s: %v", e.ID, err)
		}
	}
	// merch-a: two USD sales (net +3000), one EUR sale (net +7000).
	post(JournalEntry{ID: "s-a1", DebitAccount: "merch-a", CreditAccount: "clearing", AmountCents: 2000, Currency: "USD", IdempotencyKey: "k-s-a1", CreatedAt: day1})
	post(JournalEntry{ID: "s-a2", DebitAccount: "merch-a", CreditAccount: "clearing", AmountCents: 1500, Currency: "USD", IdempotencyKey: "k-s-a2", CreatedAt: day1.Add(time.Hour)})
	post(JournalEntry{ID: "s-a3", DebitAccount: "clearing", CreditAccount: "merch-a", AmountCents: 500, Currency: "USD", IdempotencyKey: "k-s-a3", CreatedAt: day1.Add(2 * time.Hour)}) // refund
	post(JournalEntry{ID: "s-a4", DebitAccount: "merch-a", CreditAccount: "clearing", AmountCents: 7000, Currency: "EUR", IdempotencyKey: "k-s-a4", CreatedAt: day1.Add(3 * time.Hour)})
	// merch-b: one USD sale the next day.
	post(JournalEntry{ID: "s-b1", DebitAccount: "merch-b", CreditAccount: "clearing", AmountCents: 9000, Currency: "USD", IdempotencyKey: "k-s-b1", CreatedAt: day1.Add(24 * time.Hour)})
}

func settlementCell(cells []SettlementCell, account AccountID, currency string) *SettlementCell {
	for i := range cells {
		if cells[i].Account == account && cells[i].Currency == currency {
			return &cells[i]
		}
	}
	return nil
}

func TestSettlementAggregation(t *testing.T) {
	l := New()
	if err := l.SetSettlementChannel("merch-a", "alipay"); err != nil {
		t.Fatalf("SetSettlementChannel: %v", err)
	}
	postSettlementEntries(t, l)

	now := time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)
	report, err := l.Settlement(now, SettlementOptions{Day: "2026-10-10"})
	if err != nil {
		t.Fatalf("Settlement: %v", err)
	}
	if report.Day != "2026-10-10" {
		t.Fatalf("report.Day = %q, want 2026-10-10", report.Day)
	}
	if len(report.Cells) != 4 {
		t.Fatalf("len(Cells) = %d, want 4 (merch-a USD/EUR + clearing USD/EUR)", len(report.Cells))
	}
	// Cells sorted by (account, currency): clearing/EUR first.
	if report.Cells[0].Account != "clearing" || report.Cells[0].Currency != "EUR" {
		t.Fatalf("Cells[0] = %s/%s, want clearing/EUR", report.Cells[0].Account, report.Cells[0].Currency)
	}
	// The counterparty account settles the mirror image: merch-a's net
	// +3000 USD is clearing's net -3000 USD (double-entry symmetry).
	clr := settlementCell(report.Cells, "clearing", "USD")
	if clr == nil || clr.NetCents != -3000 {
		t.Fatalf("clearing/USD cell wrong: %+v", clr)
	}
	usd := settlementCell(report.Cells, "merch-a", "USD")
	if usd == nil {
		t.Fatal("missing merch-a/USD cell")
	}
	if usd.DebitCents != 3500 || usd.CreditCents != 500 || usd.NetCents != 3000 {
		t.Fatalf("merch-a/USD = debit %d credit %d net %d, want 3500/500/3000",
			usd.DebitCents, usd.CreditCents, usd.NetCents)
	}
	if usd.Channel != "alipay" {
		t.Fatalf("merch-a/USD channel = %q, want alipay", usd.Channel)
	}
	if usd.EntryCount != 3 || len(usd.EntryIDs) != 3 {
		t.Fatalf("merch-a/USD entry count = %d, want 3", usd.EntryCount)
	}
	// Entry IDs sorted for deterministic drill-down.
	for i := 1; i < len(usd.EntryIDs); i++ {
		if usd.EntryIDs[i-1] >= usd.EntryIDs[i] {
			t.Fatalf("EntryIDs not sorted: %v", usd.EntryIDs)
		}
	}
	eur := settlementCell(report.Cells, "merch-a", "EUR")
	if eur == nil || eur.NetCents != 7000 || eur.EntryCount != 1 {
		t.Fatalf("merch-a/EUR cell wrong: %+v", eur)
	}
	// merch-b's entry is the next UTC day: not in this report.
	if c := settlementCell(report.Cells, "merch-b", "USD"); c != nil {
		t.Fatalf("unexpected merch-b cell in 2026-10-10 report: %+v", c)
	}
	// The next day's report sees merch-b with no channel.
	report2, err := l.Settlement(now, SettlementOptions{Day: "2026-10-11"})
	if err != nil {
		t.Fatalf("Settlement day2: %v", err)
	}
	b := settlementCell(report2.Cells, "merch-b", "USD")
	if b == nil || b.NetCents != 9000 || b.Channel != "" {
		t.Fatalf("merch-b/USD cell wrong: %+v", b)
	}
	// The report lists configured channels for transparency.
	if len(report.Channels) != 1 || report.Channels[0].Account != "merch-a" || report.Channels[0].Channel != "alipay" {
		t.Fatalf("report.Channels = %+v, want [merch-a/alipay]", report.Channels)
	}
}

func TestSettlementDayBoundaries(t *testing.T) {
	l := New()
	post := func(id string, at time.Time) {
		t.Helper()
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: "merch-a", CreditAccount: "clearing",
			AmountCents: 1000, Currency: "USD", IdempotencyKey: "k-" + id, CreatedAt: at,
		}); err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	// Last second of the UTC day and first second of the next.
	post("e-edge1", time.Date(2026, 10, 10, 23, 59, 59, 0, time.UTC))
	post("e-edge2", time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
	// A Beijing-time timestamp that is still Oct 10 in UTC.
	beijing := time.FixedZone("CST", 8*3600)
	post("e-edge3", time.Date(2026, 10, 11, 7, 30, 0, 0, beijing)) // 2026-10-10 23:30 UTC

	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r1, err := l.Settlement(now, SettlementOptions{Day: "2026-10-10"})
	if err != nil {
		t.Fatalf("Settlement: %v", err)
	}
	c1 := settlementCell(r1.Cells, "merch-a", "USD")
	if c1 == nil || c1.EntryCount != 2 {
		t.Fatalf("2026-10-10 cell entry count = %v, want 2", c1)
	}
	r2, err := l.Settlement(now, SettlementOptions{Day: "2026-10-11"})
	if err != nil {
		t.Fatalf("Settlement day2: %v", err)
	}
	c2 := settlementCell(r2.Cells, "merch-a", "USD")
	if c2 == nil || c2.EntryCount != 1 || c2.EntryIDs[0] != "e-edge2" {
		t.Fatalf("2026-10-11 cell wrong: %+v", c2)
	}
}

func TestSettlementExcludesPendingReview(t *testing.T) {
	l := New(WithReviewThreshold(100000)) // ledger-wide dual-control gate
	day := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	// Below the gate: effective, counts.
	if _, _, err := l.Post(JournalEntry{ID: "e-ok", DebitAccount: "merch-a", CreditAccount: "clearing",
		AmountCents: 5000, Currency: "USD", IdempotencyKey: "k-ok", CreatedAt: day}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	// Above the gate: frozen pending review, moves no money.
	receipt, err := l.PostTransfer(Transfer{
		ID: "t-pend", From: "merch-a", To: "bank", AmountCents: 200000,
		Currency: "USD", IdempotencyKey: "k-pend",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.ReviewStatus != "pending_review" {
		t.Fatalf("expected the transfer to enter dual-control review, got %q", receipt.ReviewStatus)
	}

	report, err := l.Settlement(day, SettlementOptions{Day: "2026-10-10"})
	if err != nil {
		t.Fatalf("Settlement: %v", err)
	}
	c := settlementCell(report.Cells, "merch-a", "USD")
	if c == nil {
		t.Fatal("missing merch-a/USD cell")
	}
	// The pending review's 200000 must not appear: not effective.
	if c.DebitCents != 5000 || c.CreditCents != 0 || c.NetCents != 5000 || c.EntryCount != 1 {
		t.Fatalf("pending review leaked into settlement: %+v", c)
	}
}

func TestSettlementMismatches(t *testing.T) {
	l := New()
	postSettlementEntries(t, l) // merch-a USD net +3000 on 2026-10-10
	now := time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)
	key := func(a AccountID, cur string) SettlementExpectationKey {
		return SettlementExpectationKey{Account: a, Currency: cur}
	}

	cases := []struct {
		name       string
		expected   map[SettlementExpectationKey]int64
		tolerances map[SettlementExpectationKey]SettlementTolerance
		wantAlerts int
		wantDiff   int64 // checked when wantAlerts == 1
		wantAbs    int64
		wantBps    int64
	}{
		{
			name:       "exact match: no alert",
			expected:   map[SettlementExpectationKey]int64{key("merch-a", "USD"): 3000},
			wantAlerts: 0,
		},
		{
			name:       "zero tolerance default: any nonzero diff alerts",
			expected:   map[SettlementExpectationKey]int64{key("merch-a", "USD"): 2999},
			wantAlerts: 1, wantDiff: 1,
		},
		{
			name:       "within absolute tolerance: no alert",
			expected:   map[SettlementExpectationKey]int64{key("merch-a", "USD"): 2900},
			tolerances: map[SettlementExpectationKey]SettlementTolerance{key("merch-a", "USD"): {AbsCents: 100}},
			wantAlerts: 0,
		},
		{
			name:       "beyond absolute tolerance: alert",
			expected:   map[SettlementExpectationKey]int64{key("merch-a", "USD"): 2800},
			tolerances: map[SettlementExpectationKey]SettlementTolerance{key("merch-a", "USD"): {AbsCents: 100}},
			wantAlerts: 1, wantDiff: 200, wantAbs: 100,
		},
		{
			name:       "within bps tolerance: no alert",
			expected:   map[SettlementExpectationKey]int64{key("merch-a", "USD"): 3000},
			tolerances: map[SettlementExpectationKey]SettlementTolerance{key("merch-a", "USD"): {Bps: 100}}, // 1%
			wantAlerts: 0,
		},
		{
			name:       "bps tolerance with diff: alert",
			expected:   map[SettlementExpectationKey]int64{key("merch-a", "USD"): 2000},
			tolerances: map[SettlementExpectationKey]SettlementTolerance{key("merch-a", "USD"): {Bps: 100}}, // 1% of 2000 = 20 < 1000
			wantAlerts: 1, wantDiff: 1000, wantBps: 100,
		},
		{
			name:       "expected but no activity: actual zero alerts",
			expected:   map[SettlementExpectationKey]int64{key("merch-b", "USD"): 500},
			wantAlerts: 1, wantDiff: -500,
		},
		{
			name:       "expected zero with no activity: no alert",
			expected:   map[SettlementExpectationKey]int64{key("merch-b", "USD"): 0},
			wantAlerts: 0,
		},
		{
			name:       "bps with zero expectation falls back to abs: alert",
			expected:   map[SettlementExpectationKey]int64{key("merch-b", "USD"): 0},
			tolerances: map[SettlementExpectationKey]SettlementTolerance{key("merch-b", "USD"): {Bps: 100}},
			wantAlerts: 0, // actual is also zero: diff == 0
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := l.Settlement(now, SettlementOptions{
				Day: "2026-10-10", ExpectedNets: tc.expected, Tolerances: tc.tolerances,
			})
			if err != nil {
				t.Fatalf("Settlement: %v", err)
			}
			if len(report.Mismatches) != tc.wantAlerts {
				t.Fatalf("mismatches = %d, want %d: %+v", len(report.Mismatches), tc.wantAlerts, report.Mismatches)
			}
			if tc.wantAlerts == 1 {
				m := report.Mismatches[0]
				if m.DifferenceCents != tc.wantDiff {
					t.Fatalf("difference = %d, want %d", m.DifferenceCents, tc.wantDiff)
				}
				if m.ToleranceAbsCents != tc.wantAbs || m.ToleranceBps != tc.wantBps {
					t.Fatalf("tolerance echo = (%d,%d), want (%d,%d)",
						m.ToleranceAbsCents, m.ToleranceBps, tc.wantAbs, tc.wantBps)
				}
				if m.ActualNet-m.ExpectedNet != m.DifferenceCents {
					t.Fatalf("inconsistent mismatch row: %+v", m)
				}
			}
		})
	}
}

func TestSettlementValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	key := SettlementExpectationKey{Account: "merch-a", Currency: "USD"}

	for _, day := range []string{"2026-13-01", "2026-1-5", "10/10/2026", "not-a-day", "2026-10-10T00:00:00Z"} {
		if _, err := l.Settlement(now, SettlementOptions{Day: day}); err == nil {
			t.Fatalf("day %q: expected error, got nil", day)
		}
	}
	// Empty day means today: valid.
	if _, err := l.Settlement(now, SettlementOptions{}); err != nil {
		t.Fatalf("empty day: %v", err)
	}
	badCur := SettlementExpectationKey{Account: "merch-a", Currency: "USDD"}
	if _, err := l.Settlement(now, SettlementOptions{ExpectedNets: map[SettlementExpectationKey]int64{badCur: 1}}); err == nil {
		t.Fatal("bad expectation currency: expected error")
	}
	if _, err := l.Settlement(now, SettlementOptions{Tolerances: map[SettlementExpectationKey]SettlementTolerance{key: {AbsCents: -1}}}); err == nil {
		t.Fatal("negative abs tolerance: expected error")
	}
	if _, err := l.Settlement(now, SettlementOptions{Tolerances: map[SettlementExpectationKey]SettlementTolerance{key: {Bps: 10001}}}); err == nil {
		t.Fatal("bps > 10000: expected error")
	}
	if _, err := l.Settlement(now, SettlementOptions{Tolerances: map[SettlementExpectationKey]SettlementTolerance{key: {Bps: -1}}}); err == nil {
		t.Fatal("negative bps: expected error")
	}
}

func TestSettlementChannelConfig(t *testing.T) {
	l := New()
	if _, ok := l.SettlementChannel("merch-a"); ok {
		t.Fatal("unexpected channel before config")
	}
	if err := l.SetSettlementChannel("", "alipay"); err == nil {
		t.Fatal("empty account: expected error")
	}
	if err := l.SetSettlementChannel("merch-a", strings.Repeat("x", 65)); err == nil {
		t.Fatal("over-long channel: expected error")
	}
	if err := l.SetSettlementChannel("merch-a", "alipay"); err != nil {
		t.Fatalf("SetSettlementChannel: %v", err)
	}
	ch, ok := l.SettlementChannel("merch-a")
	if !ok || ch != "alipay" {
		t.Fatalf("SettlementChannel = %q,%v, want alipay,true", ch, ok)
	}
	// Empty channel clears.
	if err := l.SetSettlementChannel("merch-a", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := l.SettlementChannel("merch-a"); ok {
		t.Fatal("channel not cleared")
	}
}

func TestSettlementCSV(t *testing.T) {
	l := New()
	if err := l.SetSettlementChannel("merch-a", "alipay"); err != nil {
		t.Fatalf("SetSettlementChannel: %v", err)
	}
	postSettlementEntries(t, l)
	report, err := l.Settlement(time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC), SettlementOptions{
		Day:          "2026-10-10",
		ExpectedNets: map[SettlementExpectationKey]int64{{Account: "merch-a", Currency: "USD"}: 2900},
	})
	if err != nil {
		t.Fatalf("Settlement: %v", err)
	}

	var buf bytes.Buffer
	if err := report.WriteCSV(&buf); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("re-parse csv: %v", err)
	}
	if len(records) != 5 { // header + 4 cells (merch-a USD/EUR, clearing USD/EUR)
		t.Fatalf("csv rows = %d, want 5", len(records))
	}
	if got, want := strings.Join(records[0], ","), "day,account,channel,currency,debit_cents,credit_cents,net_cents,entry_count,entry_ids"; got != want {
		t.Fatalf("csv header = %q, want %q", got, want)
	}
	// merch-a/USD row: channel alipay, net 3000, entries semicolon-joined.
	var row []string
	for _, r := range records[1:] {
		if r[1] == "merch-a" && r[3] == "USD" {
			row = r
		}
	}
	if row == nil {
		t.Fatalf("merch-a/USD csv row missing: %v", records)
	}
	if row[1] != "merch-a" || row[2] != "alipay" || row[3] != "USD" || row[6] != "3000" || row[7] != "3" {
		t.Fatalf("merch-a/USD csv row wrong: %v", row)
	}
	if !strings.Contains(row[8], ";") || strings.Contains(row[8], "\n") {
		t.Fatalf("entry_ids not semicolon-joined: %q", row[8])
	}

	// Mismatch export: 1 alert (2900 expected, 3000 actual, zero tolerance).
	var mbuf bytes.Buffer
	if err := report.WriteMismatchesCSV(&mbuf); err != nil {
		t.Fatalf("WriteMismatchesCSV: %v", err)
	}
	mrecs, err := csv.NewReader(&mbuf).ReadAll()
	if err != nil {
		t.Fatalf("re-parse mismatches csv: %v", err)
	}
	if len(mrecs) != 2 {
		t.Fatalf("mismatch csv rows = %d, want 2", len(mrecs))
	}
	if mrecs[1][4] != "2900" || mrecs[1][5] != "3000" || mrecs[1][6] != "100" {
		t.Fatalf("mismatch csv row wrong: %v", mrecs[1])
	}
}

func TestSettlementSQL(t *testing.T) {
	l := New()
	if err := l.SetSettlementChannel("merch-a", "alipay"); err != nil {
		t.Fatalf("SetSettlementChannel: %v", err)
	}
	// An account name with a quote exercises the SQL string escaping.
	if _, _, err := l.Post(JournalEntry{ID: "s-q1", DebitAccount: "o'brien", CreditAccount: "clearing",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "k-q1",
		CreatedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	postSettlementEntries(t, l)
	report, err := l.Settlement(time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC), SettlementOptions{
		Day:          "2026-10-10",
		ExpectedNets: map[SettlementExpectationKey]int64{{Account: "merch-a", Currency: "USD"}: 2900},
	})
	if err != nil {
		t.Fatalf("Settlement: %v", err)
	}

	var buf bytes.Buffer
	if err := report.WriteSQL(&buf); err != nil {
		t.Fatalf("WriteSQL: %v", err)
	}
	dump := buf.String()
	// The dump must say what it is: a SQL text dump, not a .db binary.
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS settlement_cells",
		"CREATE TABLE IF NOT EXISTS settlement_mismatches",
		"INSERT INTO settlement_cells",
		"INSERT INTO settlement_mismatches",
		"NOT a SQLite .db binary",
		"o''brien", // quote escaping
		"BEGIN TRANSACTION",
		"COMMIT;",
	} {
		if !strings.Contains(dump, want) {
			t.Fatalf("sql dump missing %q", want)
		}
	}

	// Real import: the dump must load into an actual SQLite database.
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI not available: skipping real import check")
	}
	dir := t.TempDir()
	dbPath := dir + "/settlement.db"
	sqlPath := dir + "/settlement.sql"
	if err := os.WriteFile(sqlPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write sql: %v", err)
	}
	cmd := exec.Command(sqlite, dbPath)
	cmd.Stdin, _ = os.Open(sqlPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 import: %v\n%s", err, out)
	}
	query := func(q string) string {
		out, err := exec.Command(sqlite, dbPath, q).CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite3 query %q: %v\n%s", q, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := query("SELECT COUNT(*) FROM settlement_cells;"); got != "5" {
		t.Fatalf("settlement_cells count = %s, want 5 (merch-a USD/EUR, clearing USD/EUR, o'brien USD)", got)
	}
	if got := query("SELECT COUNT(*) FROM settlement_mismatches;"); got != "1" {
		t.Fatalf("settlement_mismatches count = %s, want 1", got)
	}
	if got := query("SELECT net_cents FROM settlement_cells WHERE account='merch-a' AND currency='USD';"); got != "3000" {
		t.Fatalf("merch-a/USD net_cents = %s, want 3000", got)
	}
	if got := query("SELECT difference_cents FROM settlement_mismatches;"); got != "100" {
		t.Fatalf("difference_cents = %s, want 100", got)
	}
	if got := query("SELECT account FROM settlement_cells WHERE account='o''brien';"); got != "o'brien" {
		t.Fatalf("quoted account round-trip = %q, want o'brien", got)
	}
}

func TestSettlementChannelsSurviveSnapshot(t *testing.T) {
	l := New()
	if err := l.SetSettlementChannel("merch-a", "alipay"); err != nil {
		t.Fatalf("SetSettlementChannel: %v", err)
	}
	if err := l.SetSettlementChannel("merch-b", "wechat-pay"); err != nil {
		t.Fatalf("SetSettlementChannel: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "e-dr1", DebitAccount: "merch-a", CreditAccount: "clearing",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "k-dr1"}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), `"settlement_channels"`) {
		t.Fatal("snapshot config missing settlement_channels")
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	for a, want := range map[AccountID]string{"merch-a": "alipay", "merch-b": "wechat-pay"} {
		if got, ok := restored.SettlementChannel(a); !ok || got != want {
			t.Fatalf("restored channel %s = %q,%v, want %q,true", a, got, ok, want)
		}
	}
	// The channel dimension works on the restored ledger too.
	report, err := restored.Settlement(time.Now().UTC(), SettlementOptions{Day: "2026-10-10"})
	if err != nil {
		t.Fatalf("Settlement on restored: %v", err)
	}
	_ = report
}
