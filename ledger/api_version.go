package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// API version negotiation (LG-155).
//
// The ledger HTTP API versions its public surface two ways, and both
// carry the same semantics:
//
//   - Path prefix:   GET /v1/entries      (explicit, preferred for servers)
//   - Accept header: Accept: application/vnd.ledger.v1+json
//     (explicit, preferred for browser/client negotiation)
//
// A request that names no version at all keeps the legacy behavior
// exactly: it is served the latest stable version (currently v1) with
// the legacy bare-JSON response shape, so every existing integration
// keeps working. A request that names a version explicitly gets the
// versioned envelope {"api_version": "v1", "data": ...} around every
// JSON response body; non-JSON surfaces (Prometheus exposition on
// /metrics, CSV/SQL exports, JSONL audit streams, SSE) keep their own
// content types untouched and carry the resolved version in the
// X-API-Version header instead.
//
// The version route table — supported versions, media types, and the
// deprecation registry — is declared centrally here. The HTTP layer
// (main.go apiVersionMiddleware) consults it; nothing else needs to
// know about versioning.

// APIVersionV1 is the first stable API version: the entire current
// surface, unchanged in behavior, plus the versioned envelope for
// explicitly-versioned requests.
const APIVersionV1 = "v1"

// APIVersionLatest is the version served to requests that name no
// version. It advances only when a new stable version is cut; the
// legacy bare-JSON shape is preserved for unversioned requests as
// long as the latest version stays backward compatible with it.
const APIVersionLatest = APIVersionV1

// SupportedAPIVersions lists every version the server will serve,
// oldest first. Version strings are the path segment / Accept suffix,
// e.g. "v1".
var SupportedAPIVersions = []string{APIVersionV1}

// supportedAPIVersion reports whether v is a servable version.
func supportedAPIVersion(v string) bool {
	for _, s := range SupportedAPIVersions {
		if s == v {
			return true
		}
	}
	return false
}

// APIAcceptType returns the vendor media type for a version, e.g.
// "application/vnd.ledger.v1+json".
func APIAcceptType(version string) string {
	return "application/vnd.ledger." + version + "+json"
}

// ErrUnknownAPIVersion is returned by ParseAPIVersion when the request
// names a version the server does not serve. HTTP maps it to 406.
var ErrUnknownAPIVersion = errors.New("ledger: unknown API version")

// ErrConflictingAPIVersion is returned by ParseAPIVersion when the
// path prefix and the Accept header name different versions. HTTP
// maps it to 406.
var ErrConflictingAPIVersion = errors.New("ledger: path version and Accept version conflict")

// apiVersionSegment reports whether seg looks like a version segment
// ("v" followed by digits), e.g. "v1", "v12".
func apiVersionSegment(seg string) bool {
	if len(seg) < 2 || seg[0] != 'v' {
		return false
	}
	for _, c := range seg[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// acceptAPIVersion scans an Accept header value for
// application/vnd.ledger.vN+json tokens and returns the distinct
// versions named, in first-seen order. Parameters (e.g. ";q=0.9") are
// ignored; the match is case-insensitive per RFC 9110. A bare
// "application/json" or any other media type is not a version signal.
func acceptAPIVersion(accept string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		media = strings.ToLower(media)
		const prefix = "application/vnd.ledger.v"
		const suffix = "+json"
		if !strings.HasPrefix(media, prefix) || !strings.HasSuffix(media, suffix) {
			continue
		}
		digits := media[len(prefix) : len(media)-len(suffix)]
		if digits == "" {
			continue
		}
		ok := true
		for _, c := range digits {
			if c < '0' || c > '9' {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		v := "v" + digits
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ParseAPIVersion resolves the API version for a request from its path
// and Accept header. It returns the resolved version, the path with
// any version prefix stripped (for downstream routing), and whether
// the client explicitly named a version (path prefix or Accept).
//
// Resolution order: an explicit /vN/ path prefix wins over the Accept
// header; both naming different versions is a conflict (fail-closed,
// the client must disambiguate); naming no version serves the latest
// stable. A named version that is not supported is
// ErrUnknownAPIVersion.
func ParseAPIVersion(path, accept string) (version, strippedPath string, explicit bool, err error) {
	var pathVersion string
	rest := path
	if strings.HasPrefix(rest, "/") {
		seg, remainder, _ := strings.Cut(rest[1:], "/")
		if apiVersionSegment(seg) {
			pathVersion = seg
			rest = "/" + remainder
		}
	}
	acceptVersions := acceptAPIVersion(accept)
	var acceptVersion string
	switch len(acceptVersions) {
	case 0:
		// no version signal in Accept
	case 1:
		acceptVersion = acceptVersions[0]
	default:
		return "", path, false, fmt.Errorf("%w: Accept names %v", ErrConflictingAPIVersion, acceptVersions)
	}

	switch {
	case pathVersion != "" && acceptVersion != "" && pathVersion != acceptVersion:
		return "", path, false, fmt.Errorf("%w: path %q vs Accept %q",
			ErrConflictingAPIVersion, pathVersion, acceptVersion)
	case pathVersion != "":
		if !supportedAPIVersion(pathVersion) {
			return "", path, true, fmt.Errorf("%w: %q (supported: %v)",
				ErrUnknownAPIVersion, pathVersion, SupportedAPIVersions)
		}
		return pathVersion, rest, true, nil
	case acceptVersion != "":
		if !supportedAPIVersion(acceptVersion) {
			return "", path, true, fmt.Errorf("%w: %q (supported: %v)",
				ErrUnknownAPIVersion, acceptVersion, SupportedAPIVersions)
		}
		return acceptVersion, rest, true, nil
	default:
		return APIVersionLatest, path, false, nil
	}
}

// APIVersionEnvelope is the versioned JSON response shape served to
// explicitly-versioned requests: {"api_version": "v1", "data": ...}.
// Data carries the exact body the legacy shape would have carried, so
// versioned clients parse data.<field> where legacy clients parse
// <field> directly.
type APIVersionEnvelope struct {
	APIVersion string `json:"api_version"`
	Data       any    `json:"data"`
}

// DeprecatedEndpoint records one API endpoint scheduled for removal.
// Deprecated endpoints stay fully functional until Sunset; they answer
// with Deprecation: true, a Sunset date (RFC 9111), and a
// Link: <successor>; rel="successor-version" header so clients can
// migrate on their own schedule.
type DeprecatedEndpoint struct {
	Method    string
	Path      string // mux-style route, e.g. "POST /entries"
	Sunset    time.Time
	Successor string
	Reason    string
}

// DeprecatedEndpoints is the central deprecation registry for the
// current version. Entries are announced deprecations — design
// decisions made with the versioning work, documented here and in
// the README, not incident reports.
var DeprecatedEndpoints = []DeprecatedEndpoint{
	{
		Method:    "POST",
		Path:      "/entries",
		Sunset:    time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC),
		Successor: "POST /entries/batch",
		Reason:    "single-entry posting is subsumed by the batch endpoint (a one-entry batch carries identical semantics and gate coverage); v2 accepts batch-form postings only",
	},
	{
		Method:    "POST",
		Path:      "/transfers/reviews/approve",
		Sunset:    time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC),
		Successor: "POST /transfers/{id}/approve",
		Reason:    "bulk approval bypasses per-transfer review scrutiny; approve transfers individually",
	},
}

// SuccessorPath returns the successor's path without the method
// prefix ("POST /entries/batch" -> "/entries/batch"), for the Link
// header which carries a URI, not a method.
func (d *DeprecatedEndpoint) SuccessorPath() string {
	if i := strings.Index(d.Successor, " "); i >= 0 {
		return d.Successor[i+1:]
	}
	return d.Successor
}

// MatchDeprecatedEndpoint returns the registry entry for a request's
// method and stripped path, or nil. Paths in the registry may carry
// {param} segments (mux style), which match any single path segment.
func MatchDeprecatedEndpoint(method, path string) *DeprecatedEndpoint {
	for i := range DeprecatedEndpoints {
		d := &DeprecatedEndpoints[i]
		if d.Method != "" && d.Method != method {
			continue
		}
		if deprecatedPathMatches(d.Path, path) {
			return d
		}
	}
	return nil
}

// deprecatedPathMatches matches a registry path (with optional
// {param} segments, mux style) against a concrete request path.
func deprecatedPathMatches(pattern, path string) bool {
	pSegs := strings.Split(strings.Trim(pattern, "/"), "/")
	rSegs := strings.Split(strings.Trim(path, "/"), "/")
	if len(pSegs) != len(rSegs) {
		return false
	}
	for i := range pSegs {
		ps := pSegs[i]
		if len(ps) >= 2 && ps[0] == '{' && ps[len(ps)-1] == '}' {
			continue
		}
		if ps != rSegs[i] {
			return false
		}
	}
	return true
}
