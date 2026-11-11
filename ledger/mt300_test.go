package ledger

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mt300TestLedger books one forward with registered BICs on both
// parties, so the export renders :82A:/:87A:/:57A:. The forward ID
// ends in 4 digits (the :22C: derivation needs them) and stays
// within the 16x :20: limit.
func mt300TestLedger(t *testing.T, id string) (*Ledger, *FXForward) {
	t.Helper()
	l := forwardTestLedger()
	if err := l.SetAccountIBAN("payer", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN payer: %v", err)
	}
	if err := l.SetAccountIBAN("payee", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN payee: %v", err)
	}
	now := time.Now().UTC()
	c := mustBookForwardLocked(t, l, forwardBooking(id, now.Add(48*time.Hour)), now)
	return l, c
}

func TestExportMT300ExactTags(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1001")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	// Block framing: sender LT (From BIC, normalized), receiver LT
	// (To BIC), MUR = :20:.
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I300NWBKGB2LXXXU3003}",
		"{3:{108:fwd-mt300-1001}}",
		"{4:",
		":15A:",
		":20:fwd-mt300-1001",
		":22A:NEWT",
		// :22C: = first 4 of From BIC (DEUT) + first 2 of To BIC
		// (NW) + last 4 digits of the forward ID (1001).
		":22C:DEUTNW1001",
		":82A:DEUTDEFF",
		":87A:NWBKGB2L",
		":15B:",
		":30T:" + mt940ValueDate(c.BookedAt),
		":30V:" + mt940ValueDate(c.SettleAt),
		":36:1,1",
		":32B:EUR110,00",
		":33B:USD100,00",
		":57A:NWBKGB2L",
		"-}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No :21:: there is no prior confirmation to reference.
	if strings.Contains(doc, ":21:") {
		t.Errorf("export must not carry :21:, body:\n%s", doc)
	}
	if strings.HasSuffix(doc, "\n") {
		t.Error("export must not end with a trailing newline")
	}
}

func TestExportMT300Amounts(t *testing.T) {
	l := forwardTestLedger()
	if err := l.SetAccountIBAN("payer", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN payer: %v", err)
	}
	if err := l.SetAccountIBAN("payee", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN payee: %v", err)
	}
	now := time.Now().UTC()
	// Rate 1/3: :32B: must be floor(10000/3) = 3333 cents, and
	// :36: the 12-decimal half-up rounding of 1/3.
	b := forwardBooking("fwd-mt300-1002", now.Add(48*time.Hour))
	b.Rate = big.NewRat(1, 3)
	mustBookForwardLocked(t, l, b, now)
	doc, err := l.ExportMT300("fwd-mt300-1002")
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	if !strings.Contains(doc, ":32B:EUR33,33") {
		t.Errorf("want :32B:EUR33,33 (floor(10000/3)), body:\n%s", doc)
	}
	if !strings.Contains(doc, ":33B:USD100,00") {
		t.Errorf("want :33B:USD100,00 (the notional), body:\n%s", doc)
	}
	if !strings.Contains(doc, ":36:0,333333333333") {
		t.Errorf("want :36:0,333333333333, body:\n%s", doc)
	}
	// :36: parses back to RateNum/RateDen within 1e-12.
	rate := mt300RateField(t, doc)
	if diff := rate - 1.0/3.0; diff < -1e-12 || diff > 1e-12 {
		t.Errorf(":36: parses to %v, want 1/3 within 1e-12", rate)
	}
	// Rate 11/10 with a notional that does not divide evenly:
	// floor(10001*11/10) = 11001.
	b2 := forwardBooking("fwd-mt300-1003", now.Add(48*time.Hour))
	b2.NotionalCents = 10001
	mustBookForwardLocked(t, l, b2, now)
	doc2, err := l.ExportMT300("fwd-mt300-1003")
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	if !strings.Contains(doc2, ":32B:EUR110,01") {
		t.Errorf("want :32B:EUR110,01 (floor(10001*11/10)), body:\n%s", doc2)
	}
	if !strings.Contains(doc2, ":33B:USD100,01") {
		t.Errorf("want :33B:USD100,01, body:\n%s", doc2)
	}
	if !strings.Contains(doc2, ":36:1,1") {
		t.Errorf("want :36:1,1 (trailing zeros trimmed, one digit kept), body:\n%s", doc2)
	}
}

// mt300RateField extracts the :36: value from doc and parses it as a
// float (comma decimal separator).
func mt300RateField(t *testing.T, doc string) float64 {
	t.Helper()
	for _, ln := range strings.Split(doc, "\n") {
		if strings.HasPrefix(ln, ":36:") {
			v, err := strconv.ParseFloat(strings.Replace(ln[len(":36:"):], ",", ".", 1), 64)
			if err != nil {
				t.Fatalf("parse :36: %q: %v", ln, err)
			}
			return v
		}
	}
	t.Fatal("no :36: field in export")
	return 0
}

func TestExportMT300Dates(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1004")
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	// :30T: is the trade date (BookedAt), :30V: the value date
	// (SettleAt) — both pinned to the stored contract.
	if !strings.Contains(doc, ":30T:"+mt940ValueDate(c.BookedAt)) {
		t.Errorf(":30T: must be the BookedAt date %s, body:\n%s", mt940ValueDate(c.BookedAt), doc)
	}
	if !strings.Contains(doc, ":30V:"+mt940ValueDate(c.SettleAt)) {
		t.Errorf(":30V: must be the SettleAt date %s, body:\n%s", mt940ValueDate(c.SettleAt), doc)
	}
}

func TestExportMT300Cancelled(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1005")
	now := time.Now().UTC()
	if _, err := l.cancelForwardLocked(c.ID, now); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := l.ExportMT300(c.ID); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("want ErrForwardNotBooked, got %v", err)
	}
}

func TestExportMT300Expired(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1006")
	// Drive the locked core at a future instant past SettleAt +
	// grace: the lazy expiry lapses the contract first, so the
	// export reports expired — the same instant the Forward getter
	// would read as cancelled/expired.
	lapseAt := c.SettleAt.Add(ForwardSettlementGracePeriod).Add(time.Second)
	l.mu.Lock()
	_, err := l.exportMT300Locked(c.ID, lapseAt)
	l.mu.Unlock()
	if !errors.Is(err, ErrForwardExpired) {
		t.Errorf("want ErrForwardExpired, got %v", err)
	}
}

func TestExportMT300Unknown(t *testing.T) {
	l, _ := mt300TestLedger(t, "fwd-mt300-1007")
	if _, err := l.ExportMT300("fwd-mt300-9999"); !errors.Is(err, ErrForwardNotFound) {
		t.Errorf("want ErrForwardNotFound, got %v", err)
	}
}

func TestExportMT300InvalidReference(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	// A forward ID over 16 characters cannot be a :20:.
	b := forwardBooking("forward-123456789", now.Add(48*time.Hour))
	mustBookForwardLocked(t, l, b, now)
	if _, err := l.ExportMT300(b.ID); !errors.Is(err, ErrInvalidMT300Export) {
		t.Errorf("over-long ID: want ErrInvalidMT300Export, got %v", err)
	}
	// A forward ID whose last 4 characters are not digits cannot
	// form a :22C: — fail loud, never emit a malformed reference.
	b2 := forwardBooking("fwd-mt300-abcd", now.Add(48*time.Hour))
	mustBookForwardLocked(t, l, b2, now)
	if _, err := l.ExportMT300(b2.ID); !errors.Is(err, ErrInvalidMT300Export) {
		t.Errorf("non-digit ID tail: want ErrInvalidMT300Export, got %v", err)
	}
}

func TestExportMT300NoBIC(t *testing.T) {
	// No registered BIC on either party: :82D:/:87D:/:57D: carry
	// the internal account IDs. The account IDs are LT-form
	// (12 alphanumeric characters) on purpose: blocks 1/2 derive
	// their LTs from the same identifiers via mt103NormalizeLT,
	// and a non-LT-form account ID (e.g. "payer") would fail that
	// derivation — an account without a registered BIC needs an
	// LT-form account ID to export at all (see the package doc).
	l := forwardTestLedger()
	now := time.Now().UTC()
	b := ForwardBooking{
		ID:            "fwd-mt300-1008",
		From:          "ACCTPAYER001",
		To:            "ACCTPAYEE001",
		FromCurrency:  "USD",
		ToCurrency:    "EUR",
		Rate:          big.NewRat(11, 10),
		NotionalCents: 10000,
		SettleAt:      now.Add(48 * time.Hour),
	}
	mustBookForwardLocked(t, l, b, now)
	doc, err := l.ExportMT300(b.ID)
	if err != nil {
		t.Fatalf("ExportMT300: %v", err)
	}
	for _, want := range []string{
		"{1:F01ACCTPAYER0010000000000}",
		"{2:I300ACCTPAYEE001U3003}",
		// :22C: = ACCT + AC + 1008.
		":22C:ACCTAC1008",
		":82D:ACCTPAYER001",
		":87D:ACCTPAYEE001",
		":57D:ACCTPAYEE001",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	for _, forbidden := range []string{":82A:", ":87A:", ":57A:"} {
		if strings.Contains(doc, forbidden) {
			t.Errorf("no-BIC export must not carry %s, body:\n%s", forbidden, doc)
		}
	}
}

func TestExportMT300NoBICNonLTAccountRefused(t *testing.T) {
	// No registered BIC, and the account IDs are not LT-form
	// ("payer" is 5 characters): block 1/2 LT derivation fails
	// loud — no malformed header is ever emitted.
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-mt300-1009", now.Add(48*time.Hour)), now)
	if _, err := l.ExportMT300("fwd-mt300-1009"); !errors.Is(err, ErrInvalidMT300Export) {
		t.Errorf("want ErrInvalidMT300Export, got %v", err)
	}
}

func TestExportMT300SettledExportable(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1010")
	mustFundForwardPayer(t, l, 100000, "USD")
	now := time.Now().UTC()
	if _, err := l.settleForwardLocked(c.ID, now.Add(49*time.Hour)); err != nil {
		t.Fatalf("settle: %v", err)
	}
	doc, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("settled forward must still export: %v", err)
	}
	if !strings.Contains(doc, ":22A:NEWT") {
		t.Errorf("settled export missing :22A:NEWT, body:\n%s", doc)
	}
}

func TestExportMT300RepeatByteIdentical(t *testing.T) {
	l, c := mt300TestLedger(t, "fwd-mt300-1011")
	first, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("first ExportMT300: %v", err)
	}
	second, err := l.ExportMT300(c.ID)
	if err != nil {
		t.Fatalf("second ExportMT300: %v", err)
	}
	if first != second {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	// The export is read-only: no version bump, no journal rows.
	if got := l.Version(); got != c.BookedVersion {
		t.Errorf("version = %d, want %d (export must not bump it)", got, c.BookedVersion)
	}
}
