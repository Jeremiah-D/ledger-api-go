package ledger

// LG-84: audit-chain external timestamp anchoring (RFC 3161).
//
// LG-44 anchors the audit-chain head with the operator's own Ed25519
// key — a self-attestation. This module anchors it to an *external*
// timestamp authority (TSA, RFC 3161): a third party countersigns
// (chain-head digest, time), so a rewritten journal cannot be
// back-dated — any wholesale rewrite breaks the TSA-signed binding
// between the old head and its timestamp, and the token is unforgeable
// without the TSA's key.
//
// Trust story, stated honestly:
//
//   - The token (a full RFC 3161 TimeStampResp) is stored in
//     tsa_anchors.jsonl in the audit directory and should be archived
//     off-site by the operator — a token kept only next to the journal
//     it witnesses can be deleted along with it. Verification is
//     meaningful only against tokens the attacker could not destroy.
//   - VerifyTimestampToken checks the token structurally (status,
//     message-imprint digest == the claimed chain head, CMS signature
//     over the signed attributes) and, when the caller supplies the
//     TSA's certificate (WithTSATrustCert / LEDGER_TSA_CERT_FILE),
//     pins the signer to that trust anchor. Without a trust anchor the
//     signature is checked against the token's own embedded
//     certificate and the result is reported as untrusted — a token
//     that verifies only against itself proves nothing about *who*
//     signed it.
//   - Anchoring never blocks posting: the background worker (see
//     StartTimestampAnchoring) anchors on its own cadence, and a TSA
//     outage surfaces as a `tsa_anchor_failed` audit event plus the
//     ledger_tsa_anchor_failures_total counter — degraded, never fatal.
//
// Wire format: hand-rolled minimal DER for exactly the RFC 3161 / CMS
// structures this module emits and consumes (TimeStampReq,
// TimeStampResp, TSTInfo, SignedData/SignerInfo for RSA and ECDSA
// signers). Standard library only (crypto/x509 parses the embedded
// signer certificates; crypto/rsa and crypto/ecdsa verify).

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// OIDs
// ---------------------------------------------------------------------------

const (
	oidSHA256            = "2.16.840.1.101.3.4.2.1"
	oidSignedData        = "1.2.840.113549.1.7.2"
	oidTSTInfo           = "1.2.840.113549.1.9.16.1.4"
	oidContentTypeAttr   = "1.2.840.113549.1.9.3"
	oidMessageDigestAttr = "1.2.840.113549.1.9.4"
	oidSHA256WithRSA     = "1.2.840.113549.1.1.11"
	oidECDSAWithSHA256   = "1.2.840.10045.4.3.2"
)

// ---------------------------------------------------------------------------
// Minimal DER writer (emits exactly what this module needs)
// ---------------------------------------------------------------------------

const (
	derTagBoolean         = 0x01
	derTagInteger         = 0x02
	derTagOctetString     = 0x04
	derTagNull            = 0x05
	derTagOID             = 0x06
	derTagUTCTime         = 0x17
	derTagGeneralizedTime = 0x18
	derTagSequence        = 0x30
	derTagSet             = 0x31
)

// derLen encodes a DER length.
func derLen(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	out := []byte{byte(0x80 | (8 - i))}
	return append(out, b[i:]...)
}

// derTLV wraps content in tag+length.
func derTLV(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, derLen(len(content))...)
	return append(out, content...)
}

func derSeq(parts ...[]byte) []byte { return derTLV(derTagSequence, bytes.Join(parts, nil)) }
func derSet(parts ...[]byte) []byte { return derTLV(derTagSet, bytes.Join(parts, nil)) }

// derInt encodes a non-negative integer.
func derInt(v *big.Int) []byte {
	if v.Sign() < 0 {
		panic("derInt: negative integers not supported")
	}
	b := v.Bytes()
	if len(b) == 0 {
		b = []byte{0}
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return derTLV(derTagInteger, b)
}

func derInt64(v int64) []byte { return derInt(big.NewInt(v)) }

// derOID encodes a dotted-decimal OID.
func derOID(oid string) []byte {
	arcs := strings.Split(oid, ".")
	if len(arcs) < 2 {
		panic("derOID: bad oid " + oid)
	}
	nums := make([]int, len(arcs))
	for i, a := range arcs {
		n, err := strconv.Atoi(a)
		if err != nil || n < 0 {
			panic("derOID: bad arc " + a)
		}
		nums[i] = n
	}
	out := []byte{byte(nums[0]*40 + nums[1])}
	for _, n := range nums[2:] {
		var enc []byte
		for v := n; ; {
			enc = append([]byte{byte(v & 0x7f)}, enc...)
			v >>= 7
			if v == 0 {
				break
			}
		}
		for i := 0; i < len(enc)-1; i++ {
			enc[i] |= 0x80
		}
		out = append(out, enc...)
	}
	return derTLV(derTagOID, out)
}

func derOctet(b []byte) []byte { return derTLV(derTagOctetString, b) }
func derNull() []byte          { return []byte{derTagNull, 0} }
func derBool(v bool) []byte    { return derTLV(derTagBoolean, []byte{boolByte(v)}) }
func boolByte(v bool) byte {
	if v {
		return 0xff
	}
	return 0
}
func derGeneralizedTime(t time.Time) []byte {
	return derTLV(derTagGeneralizedTime, []byte(t.UTC().Format("20060102150405Z")))
}

// derContext wraps content in a constructed context-specific tag [n]
// (implicit when content is already the value bytes, explicit when the
// caller passes a full TLV — both shapes occur in CMS).
func derContext(n byte, content []byte) []byte { return derTLV(0xa0|n, content) }

// derAlgorithmIdentifier builds SEQUENCE { OID sha256, NULL }.
func derAlgorithmIdentifier(oid string) []byte { return derSeq(derOID(oid), derNull()) }

// ---------------------------------------------------------------------------
// Minimal DER reader (parses exactly what this module consumes)
// ---------------------------------------------------------------------------

// derNode is one parsed TLV. raw is the complete tag+length+content
// bytes — the CMS signature is computed over the signedAttrs node's
// raw bytes exactly as they appeared on the wire.
type derNode struct {
	tag     byte
	raw     []byte
	content []byte
}

func (n derNode) constructed() bool { return n.tag&0x20 != 0 }

// parseDER parses one TLV, returning the node and the unconsumed tail.
func parseDER(b []byte) (derNode, []byte, error) {
	if len(b) < 2 {
		return derNode{}, nil, errors.New("tsa: truncated DER")
	}
	tag := b[0]
	lb := b[1]
	var hdrLen, bodyLen int
	if lb&0x80 == 0 {
		bodyLen = int(lb)
		hdrLen = 2
	} else {
		n := int(lb & 0x7f)
		if n == 0 || n > 4 || len(b) < 2+n {
			return derNode{}, nil, errors.New("tsa: bad DER length")
		}
		bodyLen = 0
		for i := 0; i < n; i++ {
			bodyLen = bodyLen<<8 | int(b[2+i])
		}
		hdrLen = 2 + n
	}
	if len(b) < hdrLen+bodyLen {
		return derNode{}, nil, errors.New("tsa: truncated DER body")
	}
	raw := b[:hdrLen+bodyLen]
	return derNode{tag: tag, raw: raw, content: raw[hdrLen:]}, b[hdrLen+bodyLen:], nil
}

// children splits a constructed node's content into child nodes.
func (n derNode) children() ([]derNode, error) {
	if !n.constructed() {
		return nil, errors.New("tsa: expected constructed node")
	}
	var out []derNode
	rest := n.content
	for len(rest) > 0 {
		c, tail, err := parseDER(rest)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		rest = tail
	}
	return out, nil
}

func (n derNode) expect(tag byte, what string) error {
	if n.tag != tag {
		return fmt.Errorf("tsa: expected %s (tag %02x), got tag %02x", what, tag, n.tag)
	}
	return nil
}

// derBigInt reads an INTEGER node's value.
func derBigInt(n derNode) (*big.Int, error) {
	if err := n.expect(derTagInteger, "INTEGER"); err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(n.content), nil
}

// derOIDString reads an OID node's dotted form.
func derOIDString(n derNode) (string, error) {
	if err := n.expect(derTagOID, "OID"); err != nil {
		return "", err
	}
	if len(n.content) == 0 {
		return "", errors.New("tsa: empty OID")
	}
	first := int(n.content[0])
	arcs := []string{strconv.Itoa(first / 40), strconv.Itoa(first % 40)}
	v := 0
	for _, b := range n.content[1:] {
		v = v<<7 | int(b&0x7f)
		if b&0x80 == 0 {
			arcs = append(arcs, strconv.Itoa(v))
			v = 0
		}
	}
	return strings.Join(arcs, "."), nil
}

// derTime reads a UTCTime/GeneralizedTime node.
func derTime(n derNode) (time.Time, error) {
	s := string(n.content)
	var layout string
	switch n.tag {
	case derTagUTCTime:
		layout = "060102150405Z"
	case derTagGeneralizedTime:
		layout = "20060102150405Z"
	default:
		return time.Time{}, errors.New("tsa: expected time")
	}
	t, err := time.Parse(layout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("tsa: bad time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// ---------------------------------------------------------------------------
// TimeStampReq / RFC 3161 client
// ---------------------------------------------------------------------------

// TimestampAuthority is an external timestamp source: given a chain-head
// digest it returns a countersignature binding (digest, time). The
// production implementation is RFC3161Client (HTTP, RFC 3161); tests
// inject fakes or the in-process test TSA (see tsa_test.go).
type TimestampAuthority interface {
	Timestamp(ctx context.Context, digest [32]byte) (TimestampToken, error)
}

// TimestampToken is one TSA attestation: the full RFC 3161
// TimeStampResp (DER) plus the TSTInfo metadata extracted from it.
// The Raw bytes are the portable evidence — everything else is a
// convenience projection, re-derivable by VerifyTimestampToken.
type TimestampToken struct {
	// Raw is the complete TimeStampResp DER: the token to archive.
	Raw []byte
	// GenTime is the TSTInfo genTime the TSA asserted.
	GenTime time.Time
	// Serial is the TSTInfo serialNumber.
	Serial []byte
}

// encodeTimeStampReq builds a TimeStampReq for a SHA-256 message imprint.
func encodeTimeStampReq(digest [32]byte, nonce *big.Int) []byte {
	imprint := derSeq(
		derAlgorithmIdentifier(oidSHA256),
		derOctet(digest[:]),
	)
	return derSeq(
		derInt64(1), // version v1
		imprint,
		derInt(nonce), // nonce
		derBool(true), // certReq: we want the signer cert embedded
	)
}

// RFC3161Client is a TimestampAuthority over HTTP (RFC 3161
// §2.4.3.1 "via HTTP"). Zero value is unusable — set URL.
type RFC3161Client struct {
	// URL is the TSA's timestamp endpoint (https recommended).
	URL string
	// HTTP is the client to use; nil gets a 30s-timeout default.
	HTTP *http.Client
	// Nonce supplies the request nonce; nil gets crypto/rand 64-bit.
	Nonce func() *big.Int
}

func (c *RFC3161Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *RFC3161Client) nonce() *big.Int {
	if c.Nonce != nil {
		return c.Nonce()
	}
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		// crypto/rand failing is fatal for a security primitive —
		// degrade loudly rather than stamping a predictable nonce.
		panic("tsa: crypto/rand failed: " + err.Error())
	}
	return n
}

// Timestamp implements TimestampAuthority.
func (c *RFC3161Client) Timestamp(ctx context.Context, digest [32]byte) (TimestampToken, error) {
	if c.URL == "" {
		return TimestampToken{}, errors.New("tsa: RFC3161Client URL is empty")
	}
	reqDER := encodeTimeStampReq(digest, c.nonce())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(reqDER))
	if err != nil {
		return TimestampToken{}, fmt.Errorf("tsa: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/timestamp-query")
	req.Header.Set("Accept", "application/timestamp-reply")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return TimestampToken{}, fmt.Errorf("tsa: POST %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return TimestampToken{}, fmt.Errorf("tsa: read reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return TimestampToken{}, fmt.Errorf("tsa: TSA returned HTTP %d", resp.StatusCode)
	}
	tok, err := parseTimestampToken(body)
	if err != nil {
		return TimestampToken{}, err
	}
	// The TSA must echo our imprint — a token for a different digest is
	// useless as an anchor for this chain head.
	if tok.imprint != digest {
		return TimestampToken{}, errors.New("tsa: TSA reply imprint does not match the requested digest")
	}
	return TimestampToken{Raw: body, GenTime: tok.genTime, Serial: tok.serial}, nil
}

// ---------------------------------------------------------------------------
// TimeStampResp parsing + verification
// ---------------------------------------------------------------------------

// parsedToken is the extracted content of a TimeStampResp.
type parsedToken struct {
	imprint      [32]byte
	genTime      time.Time
	serial       []byte
	tstDER       []byte // the TSTInfo bytes the signature covers (via messageDigest)
	signedAt     []byte // raw signedAttrs DER the signature is computed over
	sigAlg       string
	signature    []byte
	certs        []*x509.Certificate
	signerSerial *big.Int
}

// parseTimestampToken parses and structurally validates a TimeStampResp,
// returning an error for any malformed input — never a partial token.
func parseTimestampToken(respDER []byte) (*parsedToken, error) {
	root, rest, err := parseDER(respDER)
	if err != nil {
		return nil, fmt.Errorf("tsa: parse TimeStampResp: %w", err)
	}
	if len(rest) != 0 {
		return nil, errors.New("tsa: trailing bytes after TimeStampResp")
	}
	if err := root.expect(derTagSequence, "TimeStampResp"); err != nil {
		return nil, err
	}
	kids, err := root.children()
	if err != nil || len(kids) < 1 || len(kids) > 2 {
		return nil, errors.New("tsa: TimeStampResp must have 1-2 children")
	}
	// PKIStatusInfo: SEQUENCE { INTEGER status, ... }
	statusKids, err := kids[0].children()
	if err != nil || len(statusKids) < 1 {
		return nil, errors.New("tsa: bad PKIStatusInfo")
	}
	status, err := derBigInt(statusKids[0])
	if err != nil {
		return nil, err
	}
	if status.Cmp(big.NewInt(0)) != 0 && status.Cmp(big.NewInt(1)) != 0 {
		return nil, fmt.Errorf("tsa: TSA rejected the request (status %s)", status.String())
	}
	if len(kids) < 2 {
		return nil, errors.New("tsa: granted response carries no token")
	}
	// ContentInfo: SEQUENCE { OID signedData, [0] EXPLICIT SignedData }
	ciKids, err := kids[1].children()
	if err != nil || len(ciKids) != 2 {
		return nil, errors.New("tsa: bad ContentInfo")
	}
	ct, err := derOIDString(ciKids[0])
	if err != nil || ct != oidSignedData {
		return nil, fmt.Errorf("tsa: token is not SignedData (contentType %s)", ct)
	}
	if ciKids[1].tag != 0xa0 {
		return nil, errors.New("tsa: ContentInfo content must be [0] EXPLICIT")
	}
	sdNode, _, err := parseDER(ciKids[1].content)
	if err != nil {
		return nil, fmt.Errorf("tsa: parse SignedData: %w", err)
	}
	return parseSignedData(sdNode)
}

func parseSignedData(sd derNode) (*parsedToken, error) {
	if err := sd.expect(derTagSequence, "SignedData"); err != nil {
		return nil, err
	}
	kids, err := sd.children()
	if err != nil || len(kids) < 4 {
		return nil, errors.New("tsa: SignedData needs version, digestAlgorithms, encapContentInfo, signerInfos")
	}
	// encapContentInfo is child index 2: SEQUENCE { OID tstInfo, [0] EXPLICIT OCTET STRING }
	encKids, err := kids[2].children()
	if err != nil || len(encKids) != 2 {
		return nil, errors.New("tsa: bad encapContentInfo")
	}
	ect, err := derOIDString(encKids[0])
	if err != nil || ect != oidTSTInfo {
		return nil, fmt.Errorf("tsa: encapsulated content is not TSTInfo (%s)", ect)
	}
	if encKids[1].tag != 0xa0 {
		return nil, errors.New("tsa: eContent must be [0] EXPLICIT")
	}
	tstNode, _, err := parseDER(encKids[1].content)
	if err != nil {
		return nil, fmt.Errorf("tsa: parse eContent: %w", err)
	}
	if err := tstNode.expect(derTagOctetString, "TSTInfo OCTET STRING"); err != nil {
		return nil, err
	}
	tstDER := tstNode.content
	tst, err := parseTSTInfo(tstDER)
	if err != nil {
		return nil, err
	}
	tok := &parsedToken{imprint: tst.imprint, genTime: tst.genTime, serial: tst.serial, tstDER: tstDER}
	// certificates [0] IMPLICIT (optional): concatenated cert DERs.
	for _, k := range kids[3:] {
		switch {
		case k.tag == 0xa0:
			rest := k.content
			for len(rest) > 0 {
				cn, tail, err := parseDER(rest)
				if err != nil {
					return nil, fmt.Errorf("tsa: parse embedded certificate: %w", err)
				}
				cert, err := x509.ParseCertificate(cn.raw)
				if err != nil {
					return nil, fmt.Errorf("tsa: bad embedded certificate: %w", err)
				}
				tok.certs = append(tok.certs, cert)
				rest = tail
			}
		case k.tag == derTagSet:
			// signerInfos
			sis, err := k.children()
			if err != nil || len(sis) == 0 {
				return nil, errors.New("tsa: bad signerInfos")
			}
			if len(sis) > 1 {
				return nil, errors.New("tsa: multi-signer tokens not supported")
			}
			if err := parseSignerInfo(sis[0], tok); err != nil {
				return nil, err
			}
		}
	}
	if tok.signedAt == nil {
		return nil, errors.New("tsa: token has no signerInfo")
	}
	return tok, nil
}

type parsedTSTInfo struct {
	imprint [32]byte
	genTime time.Time
	serial  []byte
}

// parseTSTInfo reads version, policy, messageImprint, serialNumber,
// genTime — everything the verifier needs; the tail is ignored.
func parseTSTInfo(tstDER []byte) (*parsedTSTInfo, error) {
	root, rest, err := parseDER(tstDER)
	if err != nil {
		return nil, fmt.Errorf("tsa: parse TSTInfo: %w", err)
	}
	if len(rest) != 0 {
		return nil, errors.New("tsa: trailing bytes in TSTInfo")
	}
	kids, err := root.children()
	if err != nil || len(kids) < 5 {
		return nil, errors.New("tsa: TSTInfo needs 5+ fields")
	}
	// kids[0] version, kids[1] policy — not needed for verification.
	impKids, err := kids[2].children()
	if err != nil || len(impKids) != 2 {
		return nil, errors.New("tsa: bad messageImprint")
	}
	algKids, err := impKids[0].children()
	if err != nil || len(algKids) < 1 {
		return nil, errors.New("tsa: bad imprint hashAlgorithm")
	}
	alg, err := derOIDString(algKids[0])
	if err != nil || alg != oidSHA256 {
		return nil, fmt.Errorf("tsa: imprint hash is not SHA-256 (%s)", alg)
	}
	if err := impKids[1].expect(derTagOctetString, "hashedMessage"); err != nil {
		return nil, err
	}
	if len(impKids[1].content) != 32 {
		return nil, errors.New("tsa: hashedMessage is not 32 bytes")
	}
	var imprint [32]byte
	copy(imprint[:], impKids[1].content)
	serial, err := derBigInt(kids[3])
	if err != nil {
		return nil, fmt.Errorf("tsa: bad TSTInfo serialNumber: %w", err)
	}
	genTime, err := derTime(kids[4])
	if err != nil {
		return nil, fmt.Errorf("tsa: bad TSTInfo genTime: %w", err)
	}
	return &parsedTSTInfo{imprint: imprint, genTime: genTime, serial: serial.Bytes()}, nil
}

// parseSignerInfo extracts the signedAttrs raw bytes, the signature
// algorithm + value, and the signer serial for certificate matching.
func parseSignerInfo(si derNode, tok *parsedToken) error {
	if err := si.expect(derTagSequence, "SignerInfo"); err != nil {
		return err
	}
	kids, err := si.children()
	if err != nil || len(kids) < 5 {
		return errors.New("tsa: SignerInfo needs 5+ fields")
	}
	// kids[1] is the signer identifier: issuerAndSerialNumber SEQUENCE.
	sidKids, err := kids[1].children()
	if err != nil || len(sidKids) == 0 {
		return errors.New("tsa: bad signer identifier")
	}
	serial, err := derBigInt(sidKids[len(sidKids)-1])
	if err != nil {
		return fmt.Errorf("tsa: bad signer serialNumber: %w", err)
	}
	tok.signerSerial = serial
	// kids[2] digestAlgorithm must be SHA-256.
	dalgKids, err := kids[2].children()
	if err != nil || len(dalgKids) < 1 {
		return errors.New("tsa: bad signer digestAlgorithm")
	}
	dalg, err := derOIDString(dalgKids[0])
	if err != nil || dalg != oidSHA256 {
		return fmt.Errorf("tsa: signer digest is not SHA-256 (%s)", dalg)
	}
	// kids[3] signedAttrs [0] IMPLICIT — the signature input is these
	// exact bytes (tag 0xa0 included), per CMS.
	if kids[3].tag != 0xa0 {
		return errors.New("tsa: SignerInfo without signedAttrs is not supported")
	}
	tok.signedAt = kids[3].raw
	if err := checkSignedAttrs(kids[3], tok.tstDER); err != nil {
		return err
	}
	// kids[4] signatureAlgorithm.
	salgKids, err := kids[4].children()
	if err != nil || len(salgKids) < 1 {
		return errors.New("tsa: bad signatureAlgorithm")
	}
	salg, err := derOIDString(salgKids[0])
	if err != nil {
		return err
	}
	tok.sigAlg = salg
	// kids[5] signature OCTET STRING.
	if err := kids[5].expect(derTagOctetString, "signature"); err != nil {
		return err
	}
	tok.signature = kids[5].content
	return nil
}

// checkSignedAttrs verifies the signed attributes bind this signature
// to this TSTInfo: contentType == id-ct-TSTInfo and messageDigest ==
// SHA-256(TSTInfo DER). The attributes' own integrity is covered by
// the CMS signature over their raw bytes.
func checkSignedAttrs(attrs derNode, tstDER []byte) error {
	attrNodes, err := attrs.children()
	if err != nil || len(attrNodes) == 0 {
		return errors.New("tsa: empty signedAttrs")
	}
	wantDigest := sha256.Sum256(tstDER)
	var sawContentType, sawMessageDigest bool
	for _, a := range attrNodes {
		ak, err := a.children()
		if err != nil || len(ak) != 2 {
			return errors.New("tsa: bad Attribute")
		}
		typ, err := derOIDString(ak[0])
		if err != nil {
			return err
		}
		vals, err := ak[1].children()
		if err != nil || len(vals) == 0 {
			return errors.New("tsa: Attribute with no values")
		}
		switch typ {
		case oidContentTypeAttr:
			ct, err := derOIDString(vals[0])
			if err != nil || ct != oidTSTInfo {
				return fmt.Errorf("tsa: signedAttrs contentType is %s, not TSTInfo", ct)
			}
			sawContentType = true
		case oidMessageDigestAttr:
			if err := vals[0].expect(derTagOctetString, "messageDigest"); err != nil {
				return err
			}
			if !bytes.Equal(vals[0].content, wantDigest[:]) {
				return errors.New("tsa: signedAttrs messageDigest does not match the TSTInfo")
			}
			sawMessageDigest = true
		}
	}
	if !sawContentType || !sawMessageDigest {
		return errors.New("tsa: signedAttrs missing contentType/messageDigest")
	}
	return nil
}

// VerifiedTimestamp is the outcome of VerifyTimestampToken.
type VerifiedTimestamp struct {
	// GenTime is the TSA-asserted time of the countersignature.
	GenTime time.Time
	// Serial is the TSTInfo serialNumber.
	Serial []byte
	// Signer is the signer's certificate subject (human-readable).
	Signer string
	// Trusted is true only when the signature verified against the
	// caller-supplied trust anchor. False means the signature is valid
	// under the token's own embedded certificate but is NOT anchored
	// to a configured trust root — reported honestly, never silently.
	Trusted bool
}

// VerifyTimestampToken verifies a TimeStampResp token against an
// expected chain-head digest. It checks: the TSA granted the request,
// the imprint digest equals wantDigest, the signed attributes bind the
// signature to this exact TSTInfo, and the CMS signature verifies with
// the signer's public key. When trusted != nil the signer certificate
// must equal trusted (pinned trust anchor); otherwise the embedded
// certificate is used and Trusted reports false.
func VerifyTimestampToken(respDER []byte, wantDigest [32]byte, trusted *x509.Certificate) (VerifiedTimestamp, error) {
	tok, err := parseTimestampToken(respDER)
	if err != nil {
		return VerifiedTimestamp{}, err
	}
	if tok.imprint != wantDigest {
		return VerifiedTimestamp{}, errors.New("tsa: token imprint does not match the expected digest")
	}
	cert, err := tok.signerCert()
	if err != nil {
		return VerifiedTimestamp{}, err
	}
	out := VerifiedTimestamp{
		GenTime: tok.genTime,
		Serial:  tok.serial,
		Signer:  cert.Subject.String(),
	}
	if trusted != nil {
		if !bytes.Equal(cert.Raw, trusted.Raw) {
			return VerifiedTimestamp{}, errors.New("tsa: token signer is not the configured trust anchor")
		}
		out.Trusted = true
	}
	if err := verifyCMSSignature(cert, tok); err != nil {
		return VerifiedTimestamp{}, err
	}
	return out, nil
}

// signerCert resolves the SignerInfo to its certificate: by serial
// number when several are embedded, or the single embedded cert.
func (tok *parsedToken) signerCert() (*x509.Certificate, error) {
	if len(tok.certs) == 0 {
		return nil, errors.New("tsa: token carries no signer certificate")
	}
	if len(tok.certs) == 1 {
		return tok.certs[0], nil
	}
	for _, c := range tok.certs {
		if c.SerialNumber.Cmp(tok.signerSerial) == 0 {
			return c, nil
		}
	}
	return nil, errors.New("tsa: signer certificate not found in token")
}

// verifyCMSSignature verifies the SignerInfo signature over the raw
// signedAttrs bytes with the signer's public key (RSA PKCS#1 v1.5 or
// ECDSA, SHA-256 — the only algorithms this module accepts).
func verifyCMSSignature(cert *x509.Certificate, tok *parsedToken) error {
	h := sha256.Sum256(tok.signedAt)
	switch tok.sigAlg {
	case oidSHA256WithRSA:
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("tsa: RSA signature algorithm but non-RSA signer key")
		}
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], tok.signature)
	case oidECDSAWithSHA256:
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("tsa: ECDSA signature algorithm but non-ECDSA signer key")
		}
		if !ecdsa.VerifyASN1(pub, h[:], tok.signature) {
			return errors.New("tsa: ECDSA signature verification failed")
		}
		return nil
	default:
		return fmt.Errorf("tsa: unsupported signature algorithm %s", tok.sigAlg)
	}
}

// ---------------------------------------------------------------------------
// Anchor records: tsa_anchors.jsonl
// ---------------------------------------------------------------------------

// tsaAnchorsFileName is the anchor journal's name inside the audit
// directory (LEDGER_AUDIT_DIR), next to checkpoints.jsonl (LG-44).
const tsaAnchorsFileName = "tsa_anchors.jsonl"

// TimestampAnchor is one external timestamping of the audit-chain head:
// the TSA's countersignature over (seq, headHash) at the TSA-asserted
// time. The Token is the full TimeStampResp DER (base64) — the portable
// evidence VerifyTimestampToken re-verifies offline.
type TimestampAnchor struct {
	// Seq is the chain length at anchor time (0 anchors the genesis head).
	Seq uint64 `json:"seq"`
	// HeadHash is the hex SHA-256 of the chain head link at Seq.
	HeadHash string `json:"head_hash"`
	// AnchoredAt is when the relay received the token, UTC.
	AnchoredAt time.Time `json:"anchored_at"`
	// TSAURL is the timestamp authority that issued the token.
	TSAURL string `json:"tsa_url"`
	// GenTime is the TSA-asserted countersignature time (TSTInfo).
	GenTime time.Time `json:"gen_time"`
	// Serial is the hex TSTInfo serialNumber.
	Serial string `json:"serial"`
	// Token is the base64 TimeStampResp DER.
	Token string `json:"token"`
}

// tsaAnchorState is the Ledger's in-memory TSA anchor state. Lock order:
// l.mu -> tsa.mu (mutations take tsa.mu without holding l.mu, and every
// read path that needs both takes l.mu first) — same contract as
// anchorState in anchor.go.
type tsaAnchorState struct {
	mu        sync.Mutex
	anchors   []TimestampAnchor
	skipped   int
	loaded    bool
	trustCert *x509.Certificate
	tsaURL    string
}

// WithTSAURL configures the timestamp authority endpoint for periodic
// anchoring (see LEDGER_TSA_URL). Empty disables TSA anchoring.
func WithTSAURL(url string) Option {
	return func(l *Ledger) { l.tsa.tsaURL = url }
}

// WithTSATrustCert pins the TSA trust anchor: VerifyTimestampToken and
// the status/verify paths report Trusted only for tokens signed by
// this certificate. Without it, signatures verify against the token's
// own embedded certificate and are honestly reported untrusted.
func WithTSATrustCert(cert *x509.Certificate) Option {
	return func(l *Ledger) { l.tsa.trustCert = cert }
}

// ParseTSATrustCert parses a PEM-encoded certificate for WithTSATrustCert
// (LEDGER_TSA_CERT_FILE). It accepts a PEM block or raw DER.
func ParseTSATrustCert(pemOrDER []byte) (*x509.Certificate, error) {
	der := pemOrDER
	if block, _ := pem.Decode(pemOrDER); block != nil {
		der = block.Bytes
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tsa: parse trust certificate: %w", err)
	}
	return cert, nil
}

func (l *Ledger) ensureTSAAnchorsLoaded() {
	t := &l.tsa
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded {
		return
	}
	t.loaded = true
	dir, ok := l.AuditDir()
	if !ok {
		return
	}
	f, err := os.Open(filepath.Join(dir, tsaAnchorsFileName))
	if err != nil {
		return // no journal yet — not an error
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var a TimestampAnchor
		if err := dec.Decode(&a); err != nil {
			if err == io.EOF {
				break
			}
			t.skipped++
			continue
		}
		if a.HeadHash == "" || a.Token == "" || a.TSAURL == "" {
			t.skipped++
			continue
		}
		t.anchors = append(t.anchors, a)
	}
}

func appendTSAAnchorLine(dir string, a TimestampAnchor) error {
	f, err := os.OpenFile(filepath.Join(dir, tsaAnchorsFileName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ErrTSAURLNotConfigured is returned by AnchorTimestamp when no TSA
// endpoint was configured (see WithTSAURL / LEDGER_TSA_URL).
var ErrTSAURLNotConfigured = errors.New("ledger: TSA URL not configured (see WithTSAURL / LEDGER_TSA_URL)")

// ErrTSAAuditDirRequired is returned by AnchorTimestamp when no audit
// directory is configured: tsa_anchors.jsonl has no durable home.
var ErrTSAAuditDirRequired = errors.New("ledger: TSA anchoring requires an audit directory (LEDGER_AUDIT_DIR) as the durable home of tsa_anchors.jsonl")

// AnchorTimestamp countersigns the current audit-chain head with the
// external timestamp authority and appends the token to
// tsa_anchors.jsonl (fsync'd before returning).
//
// The chain snapshot is taken under the read lock, so a concurrent
// Post either lands fully before the anchor (and is covered) or fully
// after (covered by the next anchor) — an anchor always attests a real
// prefix of the chain, never a torn one.
//
// Idempotent for an unchanged head from the same TSA: when the latest
// anchor already covers this exact (seq, headHash, tsaURL) the existing
// anchor is returned without another TSA round-trip — a periodic job on
// a quiet ledger does not spam the TSA.
//
// Fail-closed on configuration: no TSA URL or no audit directory is an
// error. A TSA-side failure (network, rejection) is returned as an
// error too — the background worker turns those into degraded
// `tsa_anchor_failed` audit events; posting itself never waits for an
// anchor.
func (l *Ledger) AnchorTimestamp(ctx context.Context, authority TimestampAuthority) (TimestampAnchor, error) {
	if authority == nil {
		return TimestampAnchor{}, ErrTSAURLNotConfigured
	}
	l.ensureTSAAnchorsLoaded()
	dir, ok := l.AuditDir()
	if !ok {
		return TimestampAnchor{}, ErrTSAAuditDirRequired
	}
	l.mu.RLock()
	seq := uint64(len(l.chain))
	var head [32]byte
	if seq > 0 {
		head = l.chain[seq-1].hash
	}
	l.mu.RUnlock()
	headHex := hex.EncodeToString(head[:])

	t := &l.tsa
	t.mu.Lock()
	defer t.mu.Unlock()
	tsaURL := t.tsaURL
	if n := len(t.anchors); n > 0 {
		if last := t.anchors[n-1]; last.Seq == seq && last.HeadHash == headHex && last.TSAURL == tsaURL {
			return last, nil
		}
	}
	tok, err := authority.Timestamp(ctx, head)
	if err != nil {
		return TimestampAnchor{}, err
	}
	a := TimestampAnchor{
		Seq:        seq,
		HeadHash:   headHex,
		AnchoredAt: time.Now().UTC(),
		TSAURL:     tsaURL,
		GenTime:    tok.GenTime.UTC(),
		Serial:     hex.EncodeToString(tok.Serial),
		Token:      base64.StdEncoding.EncodeToString(tok.Raw),
	}
	if err := appendTSAAnchorLine(dir, a); err != nil {
		return TimestampAnchor{}, err
	}
	t.anchors = append(t.anchors, a)
	return a, nil
}

// TimestampAnchors returns the loaded TSA anchors, oldest first. The
// returned slice is a copy.
func (l *Ledger) TimestampAnchors() []TimestampAnchor {
	l.ensureTSAAnchorsLoaded()
	t := &l.tsa
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TimestampAnchor, len(t.anchors))
	copy(out, t.anchors)
	return out
}

// TimestampAnchorStatus is the operator-facing TSA anchoring status for
// GET /entries/verify.
type TimestampAnchorStatus struct {
	// Configured reports whether a TSA URL is set.
	Configured bool `json:"configured"`
	// TSAURL is the configured endpoint (empty when unconfigured).
	TSAURL string `json:"tsa_url,omitempty"`
	// Anchors is the number of stored timestamp anchors.
	Anchors int `json:"anchors"`
	// Latest is the newest anchor, if any.
	Latest *TimestampAnchor `json:"latest,omitempty"`
	// SkippedLines counts malformed journal lines skipped at load.
	SkippedLines int `json:"skipped_lines"`
	// Trusted reports whether a TSA trust anchor is configured: only
	// then can verification pin the signer.
	Trusted bool `json:"trusted"`
	// Verified is nil when there is nothing to verify (no anchors) or
	// no trust anchor is configured; otherwise it reports whether every
	// stored anchor verifies against the live chain.
	Verified *bool `json:"verified,omitempty"`
	// Error names the first verification failure, if any.
	Error string `json:"error,omitempty"`
}

// TSAURL returns the configured timestamp authority endpoint, or ""
// when TSA anchoring is unconfigured.
func (l *Ledger) TSAURL() string {
	t := &l.tsa
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tsaURL
}

// TimestampAnchorStatus returns the TSA anchoring status: configuration,
// anchor count, the latest anchor, and — when a trust anchor is
// configured — whether every stored anchor cryptographically verifies
// and still matches the live chain. A rewritten journal breaks the
// (seq, headHash) linkage the tokens attest, which is exactly the
// wholesale-rewrite attack this feature exists to detect.
func (l *Ledger) TimestampAnchorStatus() TimestampAnchorStatus {
	l.ensureTSAAnchorsLoaded()
	t := &l.tsa
	t.mu.Lock()
	n := len(t.anchors)
	skipped := t.skipped
	var latest *TimestampAnchor
	if n > 0 {
		cp := t.anchors[n-1]
		latest = &cp
	}
	configured := t.tsaURL != ""
	url := t.tsaURL
	trusted := t.trustCert != nil
	t.mu.Unlock()
	st := TimestampAnchorStatus{
		Configured:   configured,
		TSAURL:       url,
		Anchors:      n,
		Latest:       latest,
		SkippedLines: skipped,
		Trusted:      trusted,
	}
	if n == 0 || !trusted {
		return st
	}
	l.mu.RLock()
	err := l.verifyTimestampAnchorsLocked()
	l.mu.RUnlock()
	ok := err == nil
	st.Verified = &ok
	if err != nil {
		st.Error = err.Error()
	}
	return st
}

// verifyTimestampAnchorsLocked replays every loaded TSA anchor against
// the live chain and the configured trust anchor: sequence numbers must
// strictly increase, each token must cryptographically verify (pinned
// to the trust anchor), its imprint must equal the recorded head hash,
// and the recorded (seq, headHash) must still match the live chain — a
// rewritten journal fails here. Callers must hold l.mu (read or write).
func (l *Ledger) verifyTimestampAnchorsLocked() error {
	t := &l.tsa
	t.mu.Lock()
	anchors := make([]TimestampAnchor, len(t.anchors))
	copy(anchors, t.anchors)
	trust := t.trustCert
	t.mu.Unlock()
	var prevSeq uint64
	for i, a := range anchors {
		if i > 0 && a.Seq <= prevSeq {
			return fmt.Errorf("tsa: anchor %d: seq %d not strictly increasing", i, a.Seq)
		}
		prevSeq = a.Seq
		raw, err := base64.StdEncoding.DecodeString(a.Token)
		if err != nil {
			return fmt.Errorf("tsa: anchor %d: bad token encoding: %w", i, err)
		}
		var want [32]byte
		hb, err := hex.DecodeString(a.HeadHash)
		if err != nil || len(hb) != 32 {
			return fmt.Errorf("tsa: anchor %d: bad head_hash", i)
		}
		copy(want[:], hb)
		if _, err := VerifyTimestampToken(raw, want, trust); err != nil {
			return fmt.Errorf("tsa: anchor %d: token verification failed: %w", i, err)
		}
		// The token is genuine — now check the chain still says what
		// the token attests. A wholesale journal rewrite (new links at
		// the same seq) breaks this linkage.
		if a.Seq > uint64(len(l.chain)) {
			return fmt.Errorf("tsa: anchor %d: seq %d beyond current chain length %d", i, a.Seq, len(l.chain))
		}
		var head [32]byte
		if a.Seq > 0 {
			head = l.chain[a.Seq-1].hash
		}
		if hex.EncodeToString(head[:]) != a.HeadHash {
			return fmt.Errorf("tsa: anchor %d: chain head at seq %d no longer matches the anchored digest (journal rewritten?)", i, a.Seq)
		}
	}
	return nil
}

// VerifyTimestampAnchors is the offline verification entry point: it
// replays every anchor in dir/tsa_anchors.jsonl, cryptographically
// verifying each token against its recorded head hash. Sequence numbers
// must strictly increase. When trusted != nil the signer must equal the
// trust anchor and the report is trust-pinned; otherwise each token is
// verified against its own embedded certificate (reported by the
// caller via the returned count — see VerifiedTimestamp.Trusted).
// The first break names file and line, following the audit-verify
// convention. This is the "keep the tokens off-site and check them
// later" workflow: it needs no live ledger, only the anchor journal.
func VerifyTimestampAnchors(dir string, trusted *x509.Certificate) (verified int, err error) {
	f, err := os.Open(filepath.Join(dir, tsaAnchorsFileName))
	if err != nil {
		return 0, fmt.Errorf("tsa: open anchor journal: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var prevSeq uint64
	line := 0
	for {
		var a TimestampAnchor
		if derr := dec.Decode(&a); derr != nil {
			if derr == io.EOF {
				break
			}
			return verified, fmt.Errorf("tsa: %s line %d: malformed anchor: %w", tsaAnchorsFileName, line+1, derr)
		}
		line++
		if a.HeadHash == "" || a.Token == "" {
			return verified, fmt.Errorf("tsa: %s line %d: anchor missing head_hash/token", tsaAnchorsFileName, line)
		}
		if line > 1 && a.Seq <= prevSeq {
			return verified, fmt.Errorf("tsa: %s line %d: seq %d not strictly increasing", tsaAnchorsFileName, line, a.Seq)
		}
		prevSeq = a.Seq
		raw, err := base64.StdEncoding.DecodeString(a.Token)
		if err != nil {
			return verified, fmt.Errorf("tsa: %s line %d: bad token encoding: %w", tsaAnchorsFileName, line, err)
		}
		var want [32]byte
		hb, err := hex.DecodeString(a.HeadHash)
		if err != nil || len(hb) != 32 {
			return verified, fmt.Errorf("tsa: %s line %d: bad head_hash", tsaAnchorsFileName, line)
		}
		copy(want[:], hb)
		if _, err := VerifyTimestampToken(raw, want, trusted); err != nil {
			return verified, fmt.Errorf("tsa: %s line %d: token verification failed: %w", tsaAnchorsFileName, line, err)
		}
		verified++
	}
	return verified, nil
}

// ---------------------------------------------------------------------------
// Background anchoring worker
// ---------------------------------------------------------------------------

// TimestampAnchorer periodically countersigns the audit-chain head with
// the configured TSA. A TSA outage never blocks posting: failures are
// recorded as `tsa_anchor_failed` audit events and counted, and the
// worker retries on the next tick.
type TimestampAnchorer struct {
	stop     context.CancelFunc
	done     chan struct{}
	ticks    atomic.Uint64
	anchored atomic.Uint64
	failures atomic.Uint64
}

// StartTimestampAnchoring starts the background TSA anchoring worker.
// The first tick fires after one full interval (sweeper convention);
// interval <= 0 or a nil authority disables the worker and returns nil.
// Stop is idempotent.
func StartTimestampAnchoring(ctx context.Context, l *Ledger, interval time.Duration, authority TimestampAuthority, onAnchor func(TimestampAnchor)) *TimestampAnchorer {
	if interval <= 0 || authority == nil {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	a := &TimestampAnchorer{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				a.ticks.Add(1)
				anchor, err := l.AnchorTimestamp(cctx, authority)
				if err != nil {
					a.failures.Add(1)
					// Degraded, never fatal: the books don't wait for
					// the TSA. The audit event is the operator alert.
					l.emitAudit(AuditEvent{
						Op:            "tsa_anchor_failed",
						Actor:         "TSAAnchorer",
						VersionBefore: l.version,
						VersionAfter:  l.version,
						Details: map[string]any{
							"error": err.Error(),
						},
					})
					continue
				}
				a.anchored.Add(1)
				if onAnchor != nil {
					onAnchor(anchor)
				}
			}
		}
	}()
	return a
}

// Stop halts the worker; idempotent.
func (a *TimestampAnchorer) Stop() {
	if a == nil {
		return
	}
	a.stop()
	<-a.done
}

// Ticks reports how many intervals have elapsed.
func (a *TimestampAnchorer) Ticks() uint64 { return a.ticks.Load() }

// AnchoredTotal counts successful anchorings.
func (a *TimestampAnchorer) AnchoredTotal() uint64 { return a.anchored.Load() }

// FailuresTotal counts failed anchoring attempts.
func (a *TimestampAnchorer) FailuresTotal() uint64 { return a.failures.Load() }
