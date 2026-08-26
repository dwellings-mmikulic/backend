package linear

import (
	"errors"
	"net/url"
	"testing"
)

func TestParseScope_Keys(t *testing.T) {
	tests := []struct {
		query string
		key   string
		name  string
	}{
		{"", "us", "Homes for sale across the US"},
		{"state=TX", "state:tx", "Homes for sale in Texas"},
		{"city=Katy&state=tx", "city:katy|tx", "Homes for sale in Katy, TX"},
		{"city=San%20%20Antonio&state=TX", "city:san antonio|tx", "Homes for sale in San Antonio, TX"},
		{"zip=77494", "zip:77494", "Homes for sale in 77494"},
	}
	for _, tt := range tests {
		q, _ := url.ParseQuery(tt.query)
		s, err := ParseScope(q)
		if err != nil {
			t.Errorf("%q: %v", tt.query, err)
			continue
		}
		if s.Key() != tt.key {
			t.Errorf("%q: key = %q, want %q", tt.query, s.Key(), tt.key)
		}
		if s.Name() != tt.name {
			t.Errorf("%q: name = %q, want %q", tt.query, s.Name(), tt.name)
		}
		back, err := ParseKey(s.Key())
		if err != nil || back != s {
			t.Errorf("%q: ParseKey(%q) = %+v, %v; want %+v", tt.query, s.Key(), back, err, s)
		}
	}
}

func TestParseScope_Rejects(t *testing.T) {
	for _, q := range []string{"city=Katy", "zip=77494&state=tx", "zip=7749", "zip=abcde", "state=tex"} {
		v, _ := url.ParseQuery(q)
		if _, err := ParseScope(v); !errors.Is(err, ErrBadScope) {
			t.Errorf("%q: err = %v, want ErrBadScope", q, err)
		}
	}
}

func TestParseKey_Rejects(t *testing.T) {
	for _, k := range []string{
		"", "city:katy", "planet:earth",
		"zip:123", "zip:abcde", "state:", "state:zz", "state:texas",
		"city:|tx", "city:katy|zz", "city:katy|",
	} {
		if _, err := ParseKey(k); !errors.Is(err, ErrBadScope) {
			t.Errorf("%q: err = %v, want ErrBadScope", k, err)
		}
	}
}
