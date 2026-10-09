package viewer

import (
	"strings"
	"testing"
	"time"
)

func TestHousehold_IsTheIPHashIgnoringSID(t *testing.T) {
	h := NewHasher("salt", false)
	const ip = "203.0.113.5"
	if got, want := h.Household(ip), h.IDAt("", ip, time.Time{}); got != want {
		t.Errorf("Household = %s, want the ip-kind id %s", got, want)
	}
	if h.Household(ip) == h.IDAt("roku-1", ip, time.Time{}) {
		t.Error("Household must not depend on a sid")
	}
	if h.Household(ip) == h.Household("203.0.113.6") {
		t.Error("different IPs must be different households")
	}
}

func TestParseID_RoundTrip(t *testing.T) {
	id := NewHasher("salt", false).Household("203.0.113.5")
	got, err := ParseID(id.String())
	if err != nil || got != id {
		t.Fatalf("ParseID(%s) = %s, %v", id, got, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("zz", 16), strings.Repeat("ab", 17), strings.ToUpper(id.String()) + "x"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
}
