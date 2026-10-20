package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Tax/VAT report export.
//
// LG-143. Automatic posting rules (see posting_rules.go) can book tax
// legs — e.g. a VAT withholding on a merchant settlement — as derived
// journal entries whose memo is "rule:<ruleName>" and whose entry ID is
// "<triggerID>/rule:<name>". TaxReport aggregates those derived legs
// per tax rule and currency over a period, producing the input/output
// tax summary a finance team files: totals per rule, the rate snapshot
// behind each rule, and a deterministic JSON/CSV export.
//
// The report is read-only: it scans journaled entries under the read
// lock and books nothing, bumps no version, and appends no audit link.
// Rate snapshots are taken from the live rule definitions (the bps
// amount expression), so an auditor can reproduce exactly which rate
// produced each line. An optional Ed25519 signature over the report
// (reusing the LG-44 anchor key, with its own domain) lets a third
// party verify the report came from this ledger untampered.

// Tax rule kinds: output tax (collected on sales) vs input tax (paid
// on purchases, usually recoverable).
const (
	TaxRuleOutput = "output"
	TaxRuleInput  = "input"
)

// Validation errors returned by the tax report APIs.
var (
	// ErrInvalidTaxReportOptions is returned for a malformed report
	// request: an inverted period, a bad currency, or a tax rule spec
	// with a blank name or an unknown kind.
	ErrInvalidTaxReportOptions = errors.New("ledger: invalid tax report options")
)

// TaxRuleSpec designates one posting rule as a tax rule and classifies
// it: Name is the posting rule name (matched against derived-leg memos
// "rule:<name>"), Kind is TaxRuleOutput or TaxRuleInput.
type TaxRuleSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// TaxReportOptions configures one tax report.
type TaxReportOptions struct {
	// From/To bound the report period: entries with
	// From <= CreatedAt < To are included. Both are required.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Currency filters to one ISO 4217 code; empty means all
	// currencies (each line stays single-currency).
	Currency string `json:"currency,omitempty"`
	// Rules designates the posting rules treated as tax rules. An
	// empty set yields an empty report — the operator must opt tax
	// rules in explicitly, never by heuristic.
	Rules []TaxRuleSpec `json:"rules,omitempty"`
	// ExemptAccountPrefixes excludes derived legs touching an
	// exempt account (either side): tax-exempt counterparties never
	// enter the totals.
	ExemptAccountPrefixes []string `json:"exempt_account_prefixes,omitempty"`
	// IncludeSignature signs the report with the LG-44 anchor key.
	// Without a configured anchor key the report fails with
	// ErrAnchorKeyNotConfigured — a signed report is never silently
	// downgraded to unsigned.
	IncludeSignature bool `json:"include_signature,omitempty"`
}

// TaxReportLine is one aggregated line: a tax rule's total in one
// currency over the period.
type TaxReportLine struct {
	Rule       string `json:"rule"`
	Kind       string `json:"kind"`
	Currency   string `json:"currency"`
	Cents      int64  `json:"cents"`
	EntryCount int    `json:"entry_count"`
}

// TaxReport is the generated report: per-rule lines plus the
// input/output totals, the rate snapshot, and the optional signature.
type TaxReport struct {
	From             time.Time      `json:"from"`
	To               time.Time      `json:"to"`
	Currency         string         `json:"currency,omitempty"`
	GeneratedAt      time.Time      `json:"generated_at"`
	Lines            []TaxReportLine `json:"lines"`
	TotalOutputCents int64          `json:"total_output_cents"`
	TotalInputCents  int64          `json:"total_input_cents"`
	// RateSnapshots maps each tax rule to its rate in basis points at
	// report time, parsed from the rule's amount expression; -1 when
	// the rule is not bps-based or the rule is gone.
	RateSnapshots map[string]int64 `json:"rate_snapshots_bps"`
	// KeyID/Sigature carry the optional Ed25519 report signature (see
	// IncludeSignature). KeyID is the anchor key's public-key prefix.
	KeyID     string `json:"key_id,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// taxReportDomain separates tax-report signatures from every other
// signature the anchor key might produce (anchor checkpoints use
// LEDGER-ANCHOR/v1:), so a report signature can never be transplanted
// into another protocol.
var taxReportDomain = []byte("LEDGER-TAXREPORT/v1:")

// validateTaxReportOptions checks a report request structurally.
func validateTaxReportOptions(opts *TaxReportOptions) error {
	if opts.From.IsZero() || opts.To.IsZero() {
		return fmt.Errorf("%w: from and to are required", ErrInvalidTaxReportOptions)
	}
	if !opts.To.After(opts.From) {
		return fmt.Errorf("%w: to must be after from", ErrInvalidTaxReportOptions)
	}
	if opts.Currency != "" {
		c, err := normalizeCurrency(opts.Currency)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidTaxReportOptions, err)
		}
		opts.Currency = c
	}
	seen := make(map[string]bool, len(opts.Rules))
	for _, r := range opts.Rules {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("%w: tax rule name must not be blank", ErrInvalidTaxReportOptions)
		}
		if r.Kind != TaxRuleOutput && r.Kind != TaxRuleInput {
			return fmt.Errorf("%w: tax rule %q kind must be %q or %q",
				ErrInvalidTaxReportOptions, r.Name, TaxRuleOutput, TaxRuleInput)
		}
		if seen[r.Name] {
			return fmt.Errorf("%w: duplicate tax rule %q", ErrInvalidTaxReportOptions, r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// taxReportRateSnapshot parses each tax rule's rate from the installed
// posting rules: the bps amount expression of the rule's first leg.
// A rule that is not bps-based (or no longer installed) snapshots -1 —
// the auditor sees exactly what the report could and could not
// reproduce. Callers must hold l.mu (either lock suffices).
func (l *Ledger) taxReportRateSnapshotLocked(rules []TaxRuleSpec) map[string]int64 {
	byName := make(map[string]PostingRule, len(l.postingRules))
	for _, r := range l.postingRules {
		byName[r.Name] = r
	}
	out := make(map[string]int64, len(rules))
	for _, spec := range rules {
		out[spec.Name] = -1
		r, ok := byName[spec.Name]
		if !ok || len(r.Legs) == 0 {
			continue
		}
		amt, err := parsePostingRuleAmountExpr(r.Legs[0].AmountExpr)
		if err != nil || amt.bps <= 0 {
			continue
		}
		out[spec.Name] = amt.bps
	}
	return out
}

// taxReportExemptLocked reports whether an entry touches an exempt
// account on either side. Callers must hold l.mu (either lock
// suffices).
func taxReportExemptLocked(e *JournalEntry, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if strings.HasPrefix(string(e.DebitAccount), p) || strings.HasPrefix(string(e.CreditAccount), p) {
			return true
		}
	}
	return false
}

// TaxReport generates the tax report: it aggregates derived tax legs
// (memo "rule:<name>") per designated tax rule and currency over
// [From, To). The scan is read-only — no postings, no version bump,
// no audit links. Lines sort by (rule, currency) for a deterministic
// export; the rate snapshot freezes each rule's bps at generation
// time.
func (l *Ledger) TaxReport(ctx context.Context, opts TaxReportOptions) (*TaxReport, error) {
	if err := validateTaxReportOptions(&opts); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	report := &TaxReport{
		From:          opts.From,
		To:            opts.To,
		Currency:      opts.Currency,
		GeneratedAt:   now,
		RateSnapshots: make(map[string]int64),
	}
	if len(opts.Rules) == 0 {
		return report, nil
	}
	ruleKind := make(map[string]string, len(opts.Rules))
	for _, r := range opts.Rules {
		ruleKind[r.Name] = r.Kind
	}
	type aggKey struct {
		rule     string
		currency string
	}
	agg := make(map[aggKey]*TaxReportLine)
	l.mu.RLock()
	for _, e := range l.entries {
		if !strings.HasPrefix(e.Memo, "rule:") {
			continue
		}
		name := strings.TrimPrefix(e.Memo, "rule:")
		kind, ok := ruleKind[name]
		if !ok {
			continue
		}
		if e.CreatedAt.Before(opts.From) || !e.CreatedAt.Before(opts.To) {
			continue
		}
		if opts.Currency != "" && e.Currency != opts.Currency {
			continue
		}
		if taxReportExemptLocked(&e, opts.ExemptAccountPrefixes) {
			continue
		}
		k := aggKey{rule: name, currency: e.Currency}
		line, ok := agg[k]
		if !ok {
			line = &TaxReportLine{Rule: name, Kind: kind, Currency: e.Currency}
			agg[k] = line
		}
		line.Cents += e.AmountCents
		line.EntryCount++
	}
	report.RateSnapshots = l.taxReportRateSnapshotLocked(opts.Rules)
	l.mu.RUnlock()
	for _, line := range agg {
		report.Lines = append(report.Lines, *line)
		if line.Kind == TaxRuleOutput {
			report.TotalOutputCents += line.Cents
		} else {
			report.TotalInputCents += line.Cents
		}
	}
	sort.Slice(report.Lines, func(i, j int) bool {
		if report.Lines[i].Rule != report.Lines[j].Rule {
			return report.Lines[i].Rule < report.Lines[j].Rule
		}
		return report.Lines[i].Currency < report.Lines[j].Currency
	})
	if opts.IncludeSignature {
		if err := l.signTaxReport(report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// taxReportSigningBytes is the canonical byte form a tax report
// signs: domain || JSON(report without the signature fields). The
// JSON encoding is deterministic (struct field order, sorted map
// keys), so any verifier rebuilds the identical bytes.
func taxReportSigningBytes(r *TaxReport) ([]byte, error) {
	unsigned := *r
	unsigned.KeyID = ""
	unsigned.Signature = ""
	body, err := json.Marshal(unsigned)
	if err != nil {
		return nil, err
	}
	msg := make([]byte, 0, len(taxReportDomain)+len(body))
	msg = append(msg, taxReportDomain...)
	msg = append(msg, body...)
	return msg, nil
}

// signTaxReport attaches the anchor key's Ed25519 signature to the
// report. It reuses the LG-44 anchor key but a distinct domain, so the
// signature is bound to this report and this protocol. Without a
// configured key it fails with ErrAnchorKeyNotConfigured — the caller
// asked for a signed report, and a silent downgrade would be a
// compliance lie.
func (l *Ledger) signTaxReport(r *TaxReport) error {
	a := &l.anchor
	if len(a.key) == 0 {
		return ErrAnchorKeyNotConfigured
	}
	msg, err := taxReportSigningBytes(r)
	if err != nil {
		return err
	}
	r.KeyID = a.keyID
	r.Signature = hex.EncodeToString(ed25519.Sign(a.key, msg))
	return nil
}

// VerifyTaxReportSignature checks a report's Ed25519 signature against
// a public key: it rebuilds the canonical signing bytes and verifies.
// It returns false (no error) on any mismatch — a forged report is a
// verification failure, not a system error.
func VerifyTaxReportSignature(r *TaxReport, pub ed25519.PublicKey) bool {
	if r.Signature == "" || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := hex.DecodeString(r.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg, err := taxReportSigningBytes(r)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// WriteJSON writes the report as deterministic JSON.
func (r *TaxReport) WriteJSON(w io.Writer) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return err
	}
	_, err := io.Copy(w, &buf)
	return err
}

// WriteCSV writes the report lines as CSV: one header plus one row per
// (rule, currency), then a totals row. Deterministic: lines are
// already sorted.
func (r *TaxReport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	header := []string{"rule", "kind", "currency", "cents", "entry_count", "rate_bps"}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, line := range r.Lines {
		row := []string{
			line.Rule,
			line.Kind,
			line.Currency,
			fmt.Sprintf("%d", line.Cents),
			fmt.Sprintf("%d", line.EntryCount),
			fmt.Sprintf("%d", r.RateSnapshots[line.Rule]),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	totals := []string{"TOTAL", "", r.Currency, fmt.Sprintf("%d", r.TotalOutputCents+r.TotalInputCents),
		"", ""}
	if err := cw.Write(totals); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}
