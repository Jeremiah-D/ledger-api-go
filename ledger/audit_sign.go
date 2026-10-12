package ledger

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// AuditSig is the per-line HMAC signature attached to a sealed audit
// event when the audit log runs with signing keys (LG-65). The signature
// covers the whole JSONL line — the event's canonical serialization with
// the sig field itself absent — so any rewrite of the line breaks
// verification, exactly like the hash chain (LG-32) but keyed: the chain
// proves ordering and continuity, the HMAC proves the line was written by
// someone holding the operator key. The key id in the header lets the
// offline verifier pick the right key after a rotation.
type AuditSig struct {
	KeyID string `json:"kid"`
	MAC   string `json:"mac"` // hex HMAC-SHA256
}

// AuditSigningKey is one HMAC key in the audit log's signing rotation
// set. ID lands in the sig header of every line the key signs; RetiredAt
// marks a rotated-out key — it no longer signs new lines but still
// verifies old ones inside the grace window (see VerifyAuditSignatures).
type AuditSigningKey struct {
	ID  string
	Key []byte
	// RetiredAt is when the key stopped signing. Zero means active.
	RetiredAt time.Time
}

// DefaultAuditSigningGrace is how long after a key's retirement its
// signatures still verify: 24h. Every line a retired key signed was
// written before retirement, so the window only needs to cover the tail
// of the old key's lines plus clock skew — a day is generous without
// keeping a compromised key valid for long.
const DefaultAuditSigningGrace = 24 * time.Hour

// ParseAuditSigningKeys parses LEDGER_AUDIT_SIGNING_KEYS:
//
//	"id1:hexkey,id2:hexkey@2026-10-01T00:00:00Z"
//
// Each entry is `<id>:<hexkey>` with an optional `@<retiredAt RFC3339>`
// suffix marking a rotated-out key. The first non-retired key signs new
// lines (if every key is retired, the last one listed signs — a fully
// rotated config still seals rather than failing open). Key ids must be
// unique and non-empty; key material must be non-empty hex.
func ParseAuditSigningKeys(raw string) ([]AuditSigningKey, error) {
	var keys []AuditSigningKey
	seen := make(map[string]bool)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idKey := entry
		var retiredAt time.Time
		if i := strings.LastIndex(entry, "@"); i >= 0 {
			idKey = entry[:i]
			ts := entry[i+1:]
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return nil, fmt.Errorf("ledger: bad audit signing key %q: retiredAt: %w", entry, err)
			}
			retiredAt = t.UTC()
		}
		id, hexKey, ok := strings.Cut(idKey, ":")
		if !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("ledger: bad audit signing key %q: want id:hexkey[@retiredAt]", entry)
		}
		id = strings.TrimSpace(id)
		if seen[id] {
			return nil, fmt.Errorf("ledger: duplicate audit signing key id %q", id)
		}
		seen[id] = true
		key, err := hex.DecodeString(strings.TrimSpace(hexKey))
		if err != nil || len(key) == 0 {
			return nil, fmt.Errorf("ledger: bad audit signing key %q: key must be non-empty hex", id)
		}
		keys = append(keys, AuditSigningKey{ID: id, Key: key, RetiredAt: retiredAt})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("ledger: no audit signing keys parsed")
	}
	return keys, nil
}

// auditSigner signs audit lines with the primary key and verifies them
// against the whole rotation set. It is immutable after construction.
type auditSigner struct {
	primary *AuditSigningKey
	byID    map[string]*AuditSigningKey
	grace   time.Duration
}

func newAuditSigner(keys []AuditSigningKey, grace time.Duration) (*auditSigner, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("ledger: audit signing needs at least one key")
	}
	if grace < 0 {
		return nil, fmt.Errorf("ledger: audit signing grace must be >= 0, got %v", grace)
	}
	byID := make(map[string]*AuditSigningKey, len(keys))
	primary := &keys[0]
	for i := range keys {
		k := &keys[i]
		if _, dup := byID[k.ID]; dup {
			return nil, fmt.Errorf("ledger: duplicate audit signing key id %q", k.ID)
		}
		byID[k.ID] = k
		if k.RetiredAt.IsZero() {
			primary = k
			break
		}
	}
	// Every key retired: sign with the last one listed rather than
	// failing open — the operator explicitly configured keys, and an
	// unsigned line would be worse than a line signed by a retired key.
	if !primary.RetiredAt.IsZero() {
		primary = &keys[len(keys)-1]
	}
	return &auditSigner{primary: primary, byID: byID, grace: grace}, nil
}

// sign seals line — the canonical JSONL bytes of the event with the sig
// field absent — under the primary key. The key id is domain-separated
// into the MAC input, so a signature cannot be transplanted onto another
// key's line by swapping the kid header.
func (s *auditSigner) sign(line []byte) AuditSig {
	mac := hmac.New(sha256.New, s.primary.Key)
	mac.Write([]byte(s.primary.ID))
	mac.Write([]byte{0})
	mac.Write(line)
	return AuditSig{KeyID: s.primary.ID, MAC: hex.EncodeToString(mac.Sum(nil))}
}

// verifyReason recomputes the MAC for sig over line (the canonical bytes,
// sig field absent) and reports why it fails, or "" when it verifies.
// A retired key still verifies lines whose event timestamp falls inside
// the grace window after retirement — those lines were legitimately
// signed before the key rotated out.
func (s *auditSigner) verifyReason(sig *AuditSig, line []byte, eventTS time.Time) string {
	key, ok := s.byID[sig.KeyID]
	if !ok {
		return "unknown_key"
	}
	if !key.RetiredAt.IsZero() && eventTS.After(key.RetiredAt.Add(s.grace)) {
		return "key_retired"
	}
	mac := hmac.New(sha256.New, key.Key)
	mac.Write([]byte(key.ID))
	mac.Write([]byte{0})
	mac.Write(line)
	var want []byte
	want, _ = hex.DecodeString(sig.MAC)
	if len(want) != sha256.Size || !hmac.Equal(mac.Sum(nil), want) {
		return "bad_signature"
	}
	return ""
}

// AuditSigBadLine pinpoints one line VerifyAuditSignatures rejected.
// Reason is one of:
//   - "corrupt": the line is not valid JSON — a torn write, never a
//     signature problem.
//   - "unknown_key": the sig header names a key id the verifier was not
//     given — the line was signed by a key outside the rotation set.
//   - "key_retired": the signing key is retired and the event is newer
//     than retiredAt+grace — either the grace window is too short or the
//     line was signed after rotation.
//   - "bad_signature": the MAC does not recompute — the line was edited
//     in place after signing.
type AuditSigBadLine struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	TraceID string `json:"trace_id"`
	Reason  string `json:"reason"`
}

// AuditSigVerifyReport is the result of VerifyAuditSignatures. Ok is true
// only when every signed line verified. Unsigned lines are counted, not
// failed: a log that ran without keys (see the audit_signing_disabled
// alert event) is honestly unsigned, not tampered — the count tells the
// auditor how much of the trail predates signing.
type AuditSigVerifyReport struct {
	Ok            bool              `json:"ok"`
	FilesChecked  int               `json:"files_checked"`
	LinesChecked  int               `json:"lines_checked"`
	SignedLines   int               `json:"signed_lines"`
	UnsignedLines int               `json:"unsigned_lines"`
	BadLines      []AuditSigBadLine `json:"bad_lines"`
}

// VerifyAuditSignatures replays every audit file in dir — all days,
// oldest first, plain .jsonl and .gz archives — and verifies each line's
// HMAC signature against keys. It needs no ledger instance, so an auditor
// can run it on a cold copy of the directory (the "offline" verification
// tool). Unlike the hash-chain verifier it collects every bad line:
// signature failures do not cascade, so there is no first-break cutoff.
// A corrupt (unparseable) line is reported with reason "corrupt", never
// silently skipped.
func VerifyAuditSignatures(dir string, keys []AuditSigningKey, grace time.Duration) (AuditSigVerifyReport, error) {
	rep := AuditSigVerifyReport{}
	signer, err := newAuditSigner(keys, grace)
	if err != nil {
		return rep, err
	}
	files, err := listAuditFiles(dir)
	if err != nil {
		return rep, err
	}
	for _, path := range files {
		brk, read, verr := verifyAuditSignaturesFile(path, signer, &rep)
		if verr != nil {
			return rep, verr
		}
		if !read {
			continue // rotation completed mid-verify; nothing to check
		}
		rep.FilesChecked++
		rep.BadLines = append(rep.BadLines, brk...)
	}
	rep.Ok = len(rep.BadLines) == 0
	return rep, nil
}

// verifyAuditSignaturesFile checks one audit file's lines against the
// signer's rotation set, folding the counts into rep. Lines are decoded
// with UseNumber so numeric Details round-trip through the canonical
// re-serialization byte-identical to the emit-time bytes the MAC was
// computed over (the same trick the hash-chain verifier uses).
func verifyAuditSignaturesFile(path string, signer *auditSigner, rep *AuditSigVerifyReport) (bad []AuditSigBadLine, read bool, err error) {
	fh, rdr, err := openAuditReader(path)
	if err != nil {
		return nil, false, err
	}
	if fh == nil {
		return nil, false, nil
	}
	read = true
	defer fh.Close()
	if c, ok := rdr.(interface{ Close() error }); ok {
		defer c.Close()
	}
	base := filepath.Base(path)
	sc := bufio.NewScanner(rdr)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rep.LinesChecked++
		var ev AuditEvent
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil {
			bad = append(bad, AuditSigBadLine{File: base, Line: lineNo, Reason: "corrupt"})
			continue
		}
		if ev.Sig == nil {
			rep.UnsignedLines++
			continue
		}
		rep.SignedLines++
		sig := ev.Sig
		ev.Sig = nil
		canon, merr := json.Marshal(ev)
		if merr != nil {
			bad = append(bad, AuditSigBadLine{File: base, Line: lineNo, TraceID: ev.TraceID, Reason: "corrupt"})
			continue
		}
		if reason := signer.verifyReason(sig, canon, ev.Timestamp); reason != "" {
			bad = append(bad, AuditSigBadLine{File: base, Line: lineNo, TraceID: ev.TraceID, Reason: reason})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, true, fmt.Errorf("ledger: audit signature scan %s: %w", path, err)
	}
	return bad, true, nil
}
