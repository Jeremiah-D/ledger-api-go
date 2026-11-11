package ledger

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// mt304TestLedger books one forward with registered BICs on both
// parties, so the export renders :83A:/:82A:/:87A:/:53A:/:57A:.
// The custodian is the caller-given CITIUS33 BIC.
func mt304TestLedger(t *testing.T, id string) (*Ledger, *FXForward) {
	t.Helper()
	l, c := mt300TestLedger(t, id)
	return l, c
}

func mt304Opts(id string) MT304ExportOptions {
	return MT304ExportOptions{ForwardID: id, CustodianBIC: "CITIUS33"}
}

func TestExportMT304ExactTags(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-1001")
	doc, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}
	// Block framing: sender LT (From BIC, normalized), receiver LT
	// (custodian BIC), MUR = :20:.
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I304CITIUS33XXXU3003}",
		"{3:{108:fwd-mt304-1001}}",
		"{4:",
		":15A:",
		":20:fwd-mt304-1001",
		":22A:NEWT",
		":94A:ADVI",
		// The fund and the fund manager share the From identifier.
		":83A:DEUTDEFF",
		":82A:DEUTDEFF",
		":87A:NWBKGB2L",
		":15B:",
		":30T:" + mt940ValueDate(c.BookedAt),
		":30V:" + mt940ValueDate(c.SettleAt),
		":36:1,1",
		":32B:EUR110,00",
		// B1 delivery bank and B2 receiving bank are the To party.
		":53A:NWBKGB2L",
		":33B:USD100,00",
		":57A:NWBKGB2L",
		"-}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No :22C: (the current MT304 table carries no such field), no
	// :21:, and no sequence C/D/E markers.
	for _, forbidden := range []string{":22C:", ":21:", ":15C:", ":15D:", ":15E:"} {
		if strings.Contains(doc, forbidden) {
			t.Errorf("export must not carry %s, body:\n%s", forbidden, doc)
		}
	}
	if strings.HasSuffix(doc, "\n") {
		t.Error("export must not end with a trailing newline")
	}
}

func TestExportMT304Amounts(t *testing.T) {
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
	b := forwardBooking("fwd-mt304-1002", now.Add(48*time.Hour))
	b.Rate = big.NewRat(1, 3)
	mustBookForwardLocked(t, l, b, now)
	doc, err := l.ExportMT304(mt304Opts("fwd-mt304-1002"))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
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
}

func TestExportMT304ScopeINST(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-1003")
	opts := mt304Opts(c.ID)
	opts.Scope = "INST"
	doc, err := l.ExportMT304(opts)
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}
	if !strings.Contains(doc, ":94A:INST") {
		t.Errorf("want :94A:INST, body:\n%s", doc)
	}
}

func TestExportMT304InvalidScope(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-1004")
	opts := mt304Opts(c.ID)
	opts.Scope = "BOGUS"
	if _, err := l.ExportMT304(opts); !errors.Is(err, ErrInvalidMT304Export) {
		t.Errorf("bad scope: want ErrInvalidMT304Export, got %v", err)
	}
}

func TestExportMT304CustodianRequired(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-1005")
	// No custodian at all: the instruction has nowhere to go.
	if _, err := l.ExportMT304(MT304ExportOptions{ForwardID: c.ID}); !errors.Is(err, ErrInvalidMT304Export) {
		t.Errorf("missing custodian: want ErrInvalidMT304Export, got %v", err)
	}
	// A custodian that is not a valid BIC is refused, not normalized.
	opts := mt304Opts(c.ID)
	opts.CustodianBIC = "BAD"
	if _, err := l.ExportMT304(opts); !errors.Is(err, ErrInvalidMT304Export) {
		t.Errorf("bad custodian BIC: want ErrInvalidMT304Export, got %v", err)
	}
}

func TestExportMT304Cancelled(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-1006")
	now := time.Now().UTC()
	if _, err := l.cancelForwardLocked(c.ID, now); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := l.ExportMT304(mt304Opts(c.ID)); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("want ErrForwardNotBooked, got %v", err)
	}
}

func TestExportMT304Expired(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-1007")
	// Drive the locked core at a future instant past SettleAt +
	// grace: the lazy expiry lapses the contract first, so the
	// export reports expired.
	lapseAt := c.SettleAt.Add(ForwardSettlementGracePeriod).Add(time.Second)
	l.mu.Lock()
	_, err := l.exportMT304Locked(mt304Opts(c.ID), lapseAt)
	l.mu.Unlock()
	if !errors.Is(err, ErrForwardExpired) {
		t.Errorf("want ErrForwardExpired, got %v", err)
	}
}

func TestExportMT304Unknown(t *testing.T) {
	l, _ := mt304TestLedger(t, "fwd-mt304-1008")
	if _, err := l.ExportMT304(mt304Opts("fwd-mt304-9999")); !errors.Is(err, ErrForwardNotFound) {
		t.Errorf("want ErrForwardNotFound, got %v", err)
	}
}

func TestExportMT304InvalidReference(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	// A forward ID over 16 characters cannot be a :20:.
	b := forwardBooking("forward-123456789", now.Add(48*time.Hour))
	mustBookForwardLocked(t, l, b, now)
	if _, err := l.ExportMT304(mt304Opts(b.ID)); !errors.Is(err, ErrInvalidMT304Export) {
		t.Errorf("over-long ID: want ErrInvalidMT304Export, got %v", err)
	}
}

func TestExportMT304NoBIC(t *testing.T) {
	// No registered BIC on either party: :83D:/:82D:/:87D:/:53D:
	// :57D: carry the internal account IDs. The account IDs are
	// LT-form (12 alphanumeric characters) on purpose: blocks 1/2
	// derive their LTs from the same identifiers via
	// mt103NormalizeLT.
	l := forwardTestLedger()
	now := time.Now().UTC()
	b := ForwardBooking{
		ID:            "fwd-mt304-1009",
		From:          "ACCTPAYER001",
		To:            "ACCTPAYEE001",
		FromCurrency:  "USD",
		ToCurrency:    "EUR",
		Rate:          big.NewRat(11, 10),
		NotionalCents: 10000,
		SettleAt:      now.Add(48 * time.Hour),
	}
	mustBookForwardLocked(t, l, b, now)
	doc, err := l.ExportMT304(mt304Opts(b.ID))
	if err != nil {
		t.Fatalf("ExportMT304: %v", err)
	}
	for _, want := range []string{
		"{1:F01ACCTPAYER0010000000000}",
		"{2:I304CITIUS33XXXU3003}",
		":83D:ACCTPAYER001",
		":82D:ACCTPAYER001",
		":87D:ACCTPAYEE001",
		":53D:ACCTPAYEE001",
		":57D:ACCTPAYEE001",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	for _, forbidden := range []string{":83A:", ":82A:", ":87A:", ":53A:", ":57A:"} {
		if strings.Contains(doc, forbidden) {
			t.Errorf("no-BIC export must not carry %s, body:\n%s", forbidden, doc)
		}
	}
}

func TestExportMT304RepeatByteIdentical(t *testing.T) {
	l, c := mt304TestLedger(t, "fwd-mt304-1010")
	first, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("first ExportMT304: %v", err)
	}
	second, err := l.ExportMT304(mt304Opts(c.ID))
	if err != nil {
		t.Fatalf("second ExportMT304: %v", err)
	}
	if first != second {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	// The export is read-only: no version bump, no journal rows.
	if got := l.Version(); got != c.BookedVersion {
		t.Errorf("version = %d, want %d (export must not bump it)", got, c.BookedVersion)
	}
}
