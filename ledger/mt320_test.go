package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt320TestLedger builds a ledger with a funded depositor ("saver",
// registered BIC DEUTDEFF) and one 90-day, 500 bps term deposit
// opened 2026-11-01, so :30T:/:30V: = 20261101 and :30P: =
// 20270130. The deposit ID ends in 4 digits (the :22C:
// derivation needs them) and stays within the 16x :20: limit.
func mt320TestLedger(t *testing.T) (*Ledger, TermDeposit) {
	t.Helper()
	l := seedTermDepositLedger(t)
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN saver: %v", err)
	}
	opened := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-mt320-1001",
		Account:     "saver",
		AmountCents: 400_000,
		Currency:    "USD",
		TermDays:    90,
		RateBps:     500,
		OpenedAt:    opened,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	return l, td
}

func TestExportMT320ExactTags(t *testing.T) {
	l, td := mt320TestLedger(t)
	doc, err := l.ExportMT320(MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: "NWBKGB2L"})
	if err != nil {
		t.Fatalf("ExportMT320: %v", err)
	}
	// Block framing: sender LT (depositor BIC, normalized),
	// receiver LT (counterparty BIC), MUR = :20:.
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I320NWBKGB2LXXXU3003}",
		"{3:{108:td-mt320-1001}}",
		"{4:",
		":15A:",
		":20:td-mt320-1001",
		":22A:NEWT",
		":22B:CONF",
		// :22C: = DEUT + NW + 1001 (the MT300 common reference)
		// + NWBK + DE — 16 characters, [A first 4][B first 2]
		// [4 digits][B first 4][A first 2].
		":22C:DEUTNW1001NWBKDE",
		":82A:DEUTDEFF",
		":87A:NWBKGB2L",
		":15B:",
		":17R:L",
		// Dates are 8!n YYYYMMDD: opened 2026-11-01, matures
		// 2027-01-30 (90 days on).
		":30T:20261101",
		":30V:20261101",
		":30P:20270130",
		":32B:USD4000,00",
		// :34E: = 90 days x floor(400000 x 500/10000 / 365) =
		// 90 x 54 = 4860 cents.
		":34E:USD48,60",
		":37G:0,05",
		":14D:ACT/365",
		":15C:",
		":57A:NWBKGB2L",
		":15D:",
		":57A:DEUTDEFF",
		"-}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// The omitted fields must stay omitted: no prior reference,
	// no scope/contract/fund/terms, no separate settlement amount,
	// no next interest date, no single-period or day-count extra,
	// and no F/G/H/I sequences.
	for _, banned := range []string{
		":21:", ":94A:", ":21N:", ":83a:", ":83A:", ":83D:", ":77D:",
		":32H:", ":30X:", ":30F:", ":38J:",
		":15F:", ":15G:", ":15H:", ":15I:",
	} {
		if strings.Contains(doc, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, doc)
		}
	}
	if len(":22C:DEUTNW1001NWBKDE")-len(":22C:") != 16 {
		t.Error("test bug: expected :22C: value is not 16 characters")
	}
	if strings.HasSuffix(doc, "\n") {
		t.Error("export must not end with a trailing newline")
	}
}

func TestExportMT320Amounts(t *testing.T) {
	l, td := mt320TestLedger(t)
	// :34E: is pinned to the contract math: TermDays x one day's
	// interest, and :37G: to the flat rate — both recomputed here
	// from the stored deposit, not copied from the export.
	daily, err := termDepositDailyInterestCents(td)
	if err != nil {
		t.Fatalf("termDepositDailyInterestCents: %v", err)
	}
	doc, err := l.ExportMT320(MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: "NWBKGB2L"})
	if err != nil {
		t.Fatalf("ExportMT320: %v", err)
	}
	want34E := ":34E:" + td.Currency + mt940Amount(td.TermDays*daily)
	if !strings.Contains(doc, want34E) {
		t.Errorf("export missing %q, body:\n%s", want34E, doc)
	}
	if !strings.Contains(doc, ":37G:0,05") {
		t.Errorf("export missing :37G:0,05 for 500 bps, body:\n%s", doc)
	}
}

func TestExportMT320BusinessDayGateLeavesTotal(t *testing.T) {
	// The business-day-only accrual gate changes the day-by-day
	// booking rhythm, never the contracted :34E: total: a gate-on
	// export renders the same interest as a gate-off one.
	l := New(
		WithTermDepositInterestExpense("td-interest-expense"),
		WithAccrualBusinessDaysOnly(true),
	)
	seedBalance(t, l, "saver", 1_000_000)
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN saver: %v", err)
	}
	opened := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-mt320-1001", Account: "saver", AmountCents: 400_000,
		Currency: "USD", TermDays: 90, RateBps: 500, OpenedAt: opened,
	}); err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	doc, err := l.ExportMT320(MT320ExportOptions{DepositID: "td-mt320-1001", CounterpartyBIC: "NWBKGB2L"})
	if err != nil {
		t.Fatalf("ExportMT320: %v", err)
	}
	if !strings.Contains(doc, ":34E:USD48,60") {
		t.Errorf("gate-on export :34E: changed, body:\n%s", doc)
	}
}

func TestExportMT320MaturedExports(t *testing.T) {
	l, td := mt320TestLedger(t)
	// Mature the deposit (opened 2026-11-01 for 90 days): a
	// matured deposit still confirms — only an early withdrawal
	// closes the confirmation window.
	if _, err := l.MatureTermDeposit(td.ID, time.Date(2027, 1, 30, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l.ExportMT320(MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: "NWBKGB2L"}); err != nil {
		t.Errorf("ExportMT320 matured: %v", err)
	}
}

func TestExportMT320WithdrawnEarly(t *testing.T) {
	l, td := mt320TestLedger(t)
	if _, err := l.WithdrawTermDeposit(td.ID, 0, time.Now().UTC()); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	_, err := l.ExportMT320(MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestExportMT320UnknownDeposit(t *testing.T) {
	l, _ := mt320TestLedger(t)
	_, err := l.ExportMT320(MT320ExportOptions{DepositID: "td-nope", CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("err = %v, want ErrTermDepositNotFound", err)
	}
}

func TestExportMT320BadCounterpartyBIC(t *testing.T) {
	l, td := mt320TestLedger(t)
	for _, bic := range []string{"", "NWBK", "NWBKGB2LXXXX", "NWBKGB2!"} {
		_, err := l.ExportMT320(MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: bic})
		if !errors.Is(err, ErrInvalidMT320Export) {
			t.Errorf("BIC %q: err = %v, want ErrInvalidMT320Export", bic, err)
		}
	}
}

func TestExportMT320TieredDepositRejected(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-mt320-tier", Account: "saver", AmountCents: 400_000,
		Currency: "USD", TermDays: 30,
		RateTiers: []InterestTier{{MinCents: 0, RateBps: 300}, {MinCents: 200_000, RateBps: 500}},
	}); err != nil {
		t.Fatalf("OpenTermDeposit tiered: %v", err)
	}
	_, err := l.ExportMT320(MT320ExportOptions{DepositID: "td-mt320-tier", CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrInvalidMT320Export) {
		t.Errorf("err = %v, want ErrInvalidMT320Export (no single :37G:)", err)
	}
}

func TestExportMT320BadDepositID(t *testing.T) {
	l := seedTermDepositLedger(t)
	// A 17+ character ID cannot fit :20: (16x).
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-mt320-1001-long", Account: "saver", AmountCents: 100_000,
		Currency: "USD", TermDays: 30, RateBps: 500,
	}); err != nil {
		t.Fatalf("OpenTermDeposit long ID: %v", err)
	}
	_, err := l.ExportMT320(MT320ExportOptions{DepositID: "td-mt320-1001-long", CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrInvalidMT320Export) {
		t.Errorf("long ID: err = %v, want ErrInvalidMT320Export", err)
	}
	// Braces are outside the SWIFT `x` set for :20:.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td{mt320}1001", Account: "saver", AmountCents: 100_000,
		Currency: "USD", TermDays: 30, RateBps: 500,
	}); err != nil {
		t.Fatalf("OpenTermDeposit brace ID: %v", err)
	}
	_, err = l.ExportMT320(MT320ExportOptions{DepositID: "td{mt320}1001", CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrInvalidMT320Export) {
		t.Errorf("brace ID: err = %v, want ErrInvalidMT320Export", err)
	}
}

func TestExportMT320NoBICFallsBackToAccountID(t *testing.T) {
	l := seedTermDepositLedger(t)
	// "SAVER001" is an LT-form account ID (8 alphanumeric
	// characters): no registered BIC, so the depositor renders
	// :82D:/:57D: and block 1 takes the normalized LT.
	seedBalance(t, l, "SAVER001", 1_000_000)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-mt320-2002", Account: "SAVER001", AmountCents: 100_000,
		Currency: "USD", TermDays: 30, RateBps: 500,
		OpenedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	doc, err := l.ExportMT320(MT320ExportOptions{DepositID: "td-mt320-2002", CounterpartyBIC: "NWBKGB2L"})
	if err != nil {
		t.Fatalf("ExportMT320: %v", err)
	}
	for _, want := range []string{
		"{1:F01SAVER001XXX0000000000}",
		// :22C: = SAVE + NW + 2002 + NWBK + SA.
		":22C:SAVENW2002NWBKSA",
		":82D:SAVER001",
		":87A:NWBKGB2L",
		":57D:SAVER001",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, ":82A:") {
		t.Errorf("export must not carry :82A: without a registered BIC, body:\n%s", doc)
	}
}

func TestExportMT320RepeatIdentical(t *testing.T) {
	l, td := mt320TestLedger(t)
	opts := MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: "NWBKGB2L"}
	first, err := l.ExportMT320(opts)
	if err != nil {
		t.Fatalf("first ExportMT320: %v", err)
	}
	second, err := l.ExportMT320(opts)
	if err != nil {
		t.Fatalf("second ExportMT320: %v", err)
	}
	if first != second {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
