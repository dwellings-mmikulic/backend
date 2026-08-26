// Package linear serves the listing-video library as 24/7 linear HLS
// channels: a deterministic lineup per channel, materialised in the database
// in versions, and a live playlist computed per request from the wall clock.
// See docs/superpowers/specs/2026-08-25-linear-channels-design.md.
package linear

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Scope is a geographic slice of the library. The zero value is national.
// City and State are lowercase; Zip is five digits.
type Scope struct {
	Zip   string
	City  string
	State string
}

// ErrBadScope reports an invalid channel filter.
var ErrBadScope = errors.New("invalid channel filter")

// maxParamLen caps a raw filter parameter. The endpoints are
// unauthenticated and every distinct value becomes a channel key (a cache
// entry, and potentially a stored lineup chain), so absurd values are
// rejected before they are normalised.
const maxParamLen = 64

// ParseScope reads zip, city+state or state from query parameters. It is
// strict about combinations so that every distinct URL maps to one key.
func ParseScope(q url.Values) (Scope, error) {
	for _, p := range []string{"zip", "city", "state"} {
		if len(q.Get(p)) > maxParamLen {
			return Scope{}, fmt.Errorf("%w: %s is longer than %d characters", ErrBadScope, p, maxParamLen)
		}
	}
	s := Scope{
		Zip:   strings.TrimSpace(q.Get("zip")),
		City:  strings.ToLower(strings.Join(strings.Fields(q.Get("city")), " ")),
		State: strings.ToLower(strings.TrimSpace(q.Get("state"))),
	}
	switch {
	case s.Zip != "" && (s.City != "" || s.State != ""):
		return Scope{}, fmt.Errorf("%w: zip cannot be combined with city or state", ErrBadScope)
	case s.City != "" && s.State == "":
		return Scope{}, fmt.Errorf("%w: city requires state", ErrBadScope)
	case s.Zip != "" && !isZip(s.Zip):
		return Scope{}, fmt.Errorf("%w: zip must be 5 digits", ErrBadScope)
	case s.State != "" && len(s.State) != 2:
		return Scope{}, fmt.Errorf("%w: state must be a 2-letter code", ErrBadScope)
	}
	return s, nil
}

func isZip(z string) bool {
	if len(z) != 5 {
		return false
	}
	for _, r := range z {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Key is the canonical channel identifier: "us", "state:tx", "city:katy|tx"
// or "zip:77494". It is the channel_lineups.channel_key.
func (s Scope) Key() string {
	switch {
	case s.Zip != "":
		return "zip:" + s.Zip
	case s.City != "":
		return "city:" + s.City + "|" + s.State
	case s.State != "":
		return "state:" + s.State
	}
	return "us"
}

// ParseKey inverts Key. It accepts only what Key produces.
func ParseKey(k string) (Scope, error) {
	switch {
	case k == "us":
		return Scope{}, nil
	case strings.HasPrefix(k, "zip:"):
		return Scope{Zip: k[len("zip:"):]}, nil
	case strings.HasPrefix(k, "state:"):
		return Scope{State: k[len("state:"):]}, nil
	case strings.HasPrefix(k, "city:"):
		city, state, ok := strings.Cut(k[len("city:"):], "|")
		if !ok {
			return Scope{}, fmt.Errorf("%w: %q", ErrBadScope, k)
		}
		return Scope{City: city, State: state}, nil
	}
	return Scope{}, fmt.Errorf("%w: %q", ErrBadScope, k)
}

// knownState reports whether the scope's state is a real US state code. A
// national or ZIP-only scope has no state and is trivially fine.
func (s Scope) knownState() bool {
	if s.State == "" {
		return true
	}
	_, ok := stateNames[s.State]
	return ok
}

// Name is the viewer-facing channel title.
func (s Scope) Name() string {
	switch {
	case s.Zip != "":
		return "Homes for sale in " + s.Zip
	case s.City != "":
		return "Homes for sale in " + titleCase(s.City) + ", " + strings.ToUpper(s.State)
	case s.State != "":
		if n, ok := stateNames[s.State]; ok {
			return "Homes for sale in " + n
		}
		return "Homes for sale in " + strings.ToUpper(s.State)
	}
	return "Homes for sale across the US"
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

var stateNames = map[string]string{
	"al": "Alabama", "ak": "Alaska", "az": "Arizona", "ar": "Arkansas", "ca": "California",
	"co": "Colorado", "ct": "Connecticut", "de": "Delaware", "dc": "Washington, DC", "fl": "Florida",
	"ga": "Georgia", "hi": "Hawaii", "id": "Idaho", "il": "Illinois", "in": "Indiana",
	"ia": "Iowa", "ks": "Kansas", "ky": "Kentucky", "la": "Louisiana", "me": "Maine",
	"md": "Maryland", "ma": "Massachusetts", "mi": "Michigan", "mn": "Minnesota", "ms": "Mississippi",
	"mo": "Missouri", "mt": "Montana", "ne": "Nebraska", "nv": "Nevada", "nh": "New Hampshire",
	"nj": "New Jersey", "nm": "New Mexico", "ny": "New York", "nc": "North Carolina", "nd": "North Dakota",
	"oh": "Ohio", "ok": "Oklahoma", "or": "Oregon", "pa": "Pennsylvania", "ri": "Rhode Island",
	"sc": "South Carolina", "sd": "South Dakota", "tn": "Tennessee", "tx": "Texas", "ut": "Utah",
	"vt": "Vermont", "va": "Virginia", "wa": "Washington", "wv": "West Virginia", "wi": "Wisconsin",
	"wy": "Wyoming",
}
