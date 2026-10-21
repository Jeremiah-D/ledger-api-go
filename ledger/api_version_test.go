package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseAPIVersionUnversioned(t *testing.T) {
	v, stripped, explicit, err := ParseAPIVersion("/entries", "")
	if err != nil {
		t.Fatalf("ParseAPIVersion: %v", err)
	}
	if v != APIVersionLatest {
		t.Fatalf("version = %q, want latest %q", v, APIVersionLatest)
	}
	if stripped != "/entries" {
		t.Fatalf("stripped = %q, want /entries", stripped)
	}
	if explicit {
		t.Fatal("unversioned request must not be marked explicit")
	}
}

func TestParseAPIVersionPathPrefix(t *testing.T) {
	v, stripped, explicit, err := ParseAPIVersion("/v1/entries?limit=2", "application/json")
	if err != nil {
		t.Fatalf("ParseAPIVersion: %v", err)
	}
	if v != "v1" || stripped != "/entries?limit=2" || !explicit {
		t.Fatalf("got %q %q explicit=%v", v, stripped, explicit)
	}
}

func TestParseAPIVersionUnknownPathVersion(t *testing.T) {
	_, _, _, err := ParseAPIVersion("/v2/entries", "")
	if !errors.Is(err, ErrUnknownAPIVersion) {
		t.Fatalf("want ErrUnknownAPIVersion, got %v", err)
	}
}

func TestParseAPIVersionAcceptHeader(t *testing.T) {
	v, stripped, explicit, err := ParseAPIVersion("/entries", "application/vnd.ledger.v1+json")
	if err != nil {
		t.Fatalf("ParseAPIVersion: %v", err)
	}
	if v != "v1" || stripped != "/entries" || !explicit {
		t.Fatalf("got %q %q explicit=%v", v, stripped, explicit)
	}
}

func TestParseAPIVersionAcceptWithParams(t *testing.T) {
	// q-values and charset params must not break version extraction;
	// matching is case-insensitive per RFC 9110.
	v, _, explicit, err := ParseAPIVersion("/entries", "Application/VND.Ledger.V1+JSON; q=0.9, application/json")
	if err != nil {
		t.Fatalf("ParseAPIVersion: %v", err)
	}
	if v != "v1" || !explicit {
		t.Fatalf("got %q explicit=%v", v, explicit)
	}
}

func TestParseAPIVersionAcceptUnknown(t *testing.T) {
	_, _, _, err := ParseAPIVersion("/entries", "application/vnd.ledger.v9+json")
	if !errors.Is(err, ErrUnknownAPIVersion) {
		t.Fatalf("want ErrUnknownAPIVersion, got %v", err)
	}
}

func TestParseAPIVersionConflict(t *testing.T) {
	_, _, _, err := ParseAPIVersion("/v1/entries", "application/vnd.ledger.v1+json, application/vnd.ledger.v9+json")
	if !errors.Is(err, ErrConflictingAPIVersion) {
		t.Fatalf("want ErrConflictingAPIVersion for two Accept versions, got %v", err)
	}
	_, _, _, err = ParseAPIVersion("/v9/entries", "application/vnd.ledger.v1+json")
	if !errors.Is(err, ErrConflictingAPIVersion) && !errors.Is(err, ErrUnknownAPIVersion) {
		t.Fatalf("want conflict or unknown for path/Accept mismatch, got %v", err)
	}
}

func TestParseAPIVersionPlainJSONAccept(t *testing.T) {
	// A plain application/json Accept is not a version signal.
	v, _, explicit, err := ParseAPIVersion("/entries", "application/json")
	if err != nil {
		t.Fatalf("ParseAPIVersion: %v", err)
	}
	if v != APIVersionLatest || explicit {
		t.Fatalf("got %q explicit=%v", v, explicit)
	}
}

func TestParseAPIVersionNonVersionSegment(t *testing.T) {
	// "/velocity-limits" starts with 'v' but is not a version segment.
	v, stripped, explicit, err := ParseAPIVersion("/velocity-limits", "")
	if err != nil {
		t.Fatalf("ParseAPIVersion: %v", err)
	}
	if v != APIVersionLatest || stripped != "/velocity-limits" || explicit {
		t.Fatalf("got %q %q explicit=%v", v, stripped, explicit)
	}
}

func TestAcceptTypeRoundTrip(t *testing.T) {
	for _, v := range SupportedAPIVersions {
		got := acceptAPIVersion(APIAcceptType(v))
		if len(got) != 1 || got[0] != v {
			t.Fatalf("media type %q did not round-trip: %v", APIAcceptType(v), got)
		}
	}
}

func TestMatchDeprecatedEndpoint(t *testing.T) {
	d := MatchDeprecatedEndpoint("POST", "/entries")
	if d == nil || d.Successor != "POST /entries/batch" {
		t.Fatalf("POST /entries should match the deprecation registry, got %+v", d)
	}
	if !d.Sunset.After(time.Now()) {
		t.Fatalf("sunset %v must be in the future", d.Sunset)
	}
	if !strings.Contains(d.Reason, "batch") {
		t.Fatalf("reason should document the migration path: %q", d.Reason)
	}
	if MatchDeprecatedEndpoint("GET", "/entries") != nil {
		t.Fatal("GET /entries is not deprecated")
	}
	if MatchDeprecatedEndpoint("POST", "/entries/batch") != nil {
		t.Fatal("the successor must not be deprecated")
	}
	d = MatchDeprecatedEndpoint("POST", "/transfers/reviews/approve")
	if d == nil || !strings.Contains(d.Successor, "{id}") {
		t.Fatalf("batch review approval should be deprecated, got %+v", d)
	}
}

func TestDeprecatedPathTemplateMatch(t *testing.T) {
	if !deprecatedPathMatches("/transfers/{id}/approve", "/transfers/abc/approve") {
		t.Fatal("{id} should match a single segment")
	}
	if deprecatedPathMatches("/transfers/{id}/approve", "/transfers/a/b/approve") {
		t.Fatal("segment count mismatch must not match")
	}
	if !deprecatedPathMatches("/entries", "/entries/") {
		t.Fatal("a trailing slash is normalized and must still match")
	}
}

func TestDeprecatedRegistrySanity(t *testing.T) {
	// Every registered deprecation must name a supported sunset in the
	// future, a non-empty successor, and a documented reason — the
	// registry is a client-facing contract, not a TODO list.
	for _, d := range DeprecatedEndpoints {
		if d.Method == "" || d.Path == "" {
			t.Fatalf("deprecation entry missing method/path: %+v", d)
		}
		if d.Sunset.Before(time.Now()) {
			t.Fatalf("sunset for %s %s is in the past", d.Method, d.Path)
		}
		if d.Successor == "" || d.Reason == "" {
			t.Fatalf("deprecation entry for %s %s lacks successor/reason", d.Method, d.Path)
		}
	}
}
