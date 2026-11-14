package viewer

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHasher_IPSources(t *testing.T) {
	h := NewHasher("salt", false)
	req := func(remote, realIP, xff string) ID {
		r := httptest.NewRequest("GET", "/channels/live.m3u8", nil)
		r.RemoteAddr = remote
		if realIP != "" {
			r.Header.Set("X-Real-IP", realIP)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return h.ID(r)
	}
	a := req("10.0.0.1:1234", "203.0.113.5", "")
	b := req("10.0.0.9:9", "203.0.113.5", "198.51.100.1")
	if a != b {
		t.Error("X-Real-IP should win over XFF and RemoteAddr")
	}
	c := req("10.0.0.9:9", "", "203.0.113.5, 10.0.0.1")
	if c != a {
		t.Error("first XFF hop should equal the same X-Real-IP")
	}
	d := req("203.0.113.5:4444", "", "")
	if d != a {
		t.Error("RemoteAddr host (port stripped) should equal the same IP")
	}
	if req("203.0.113.6:1", "", "") == a {
		t.Error("different IPs must differ")
	}
}

func TestHasher_SIDBeatsIP(t *testing.T) {
	h := NewHasher("salt", false)
	r1 := httptest.NewRequest("GET", "/channels/live.m3u8?sid=abc", nil)
	r1.Header.Set("X-Real-IP", "203.0.113.5")
	r2 := httptest.NewRequest("GET", "/channels/live.m3u8?sid=abc", nil)
	r2.Header.Set("X-Real-IP", "203.0.113.99")
	if h.ID(r1) != h.ID(r2) {
		t.Error("same sid must hash equal regardless of IP")
	}
	r3 := httptest.NewRequest("GET", "/channels/live.m3u8", nil)
	r3.Header.Set("X-Real-IP", "abc")
	if h.ID(r1) == h.ID(r3) {
		t.Error("sid and ip namespaces must not collide")
	}
}

func TestHasher_SaltAndRotation(t *testing.T) {
	ip := "203.0.113.5"
	day1 := time.Date(2026, 8, 26, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)
	fixed := NewHasher("a", false)
	if fixed.IDAt("", ip, day1) != fixed.IDAt("", ip, day2) {
		t.Error("without rotation the id must be stable across days")
	}
	rot := NewHasher("a", true)
	if rot.IDAt("", ip, day1) == rot.IDAt("", ip, day2) {
		t.Error("with rotation the id must change at UTC midnight")
	}
	if rot.IDAt("", ip, day1) != rot.IDAt("", ip, day1.Add(-time.Hour)) {
		t.Error("with rotation the id must be stable within a day")
	}
	if NewHasher("b", false).IDAt("", ip, day1) == fixed.IDAt("", ip, day1) {
		t.Error("different salts must differ")
	}
	if fixed.IDAt("", ip, day1).String() == "" || len(fixed.IDAt("", ip, day1).String()) != 32 {
		t.Error("String should be 32 hex chars")
	}
}

func TestHasher_IPParam(t *testing.T) {
	h := trusting(t, "198.51.100.1/32")
	req := func(target, realIP string) ID {
		r := httptest.NewRequest("GET", target, nil)
		r.Header.Set("X-Real-IP", realIP)
		return h.ID(r)
	}
	// A platform macro such as ip={RokuIP} names the viewer when the request
	// itself comes from a server of that platform.
	if req("/channels/live.m3u8?ip=203.0.113.5", "198.51.100.1") != req("/channels/live.m3u8", "203.0.113.5") {
		t.Error("a valid ip parameter must identify the viewer like the client IP")
	}
	// An unfilled or garbled macro falls back to the client IP.
	for _, bad := range []string{"%7BRokuIP%7D", "", "999.1.1.1", "localhost"} {
		if req("/channels/live.m3u8?ip="+bad, "198.51.100.1") != req("/channels/live.m3u8", "198.51.100.1") {
			t.Errorf("ip=%q must be ignored", bad)
		}
	}
	// sid still wins over the ip parameter.
	if req("/channels/live.m3u8?sid=abc&ip=203.0.113.5", "1.1.1.1") != req("/channels/live.m3u8?sid=abc", "2.2.2.2") {
		t.Error("sid must win over the ip parameter")
	}
}

func TestSID(t *testing.T) {
	for in, want := range map[string]string{
		"abc":                                    "abc",
		" 5f1c2e9a-0b3d-4c1e-9a7f-2b6d8e4c1a00 ": "5f1c2e9a-0b3d-4c1e-9a7f-2b6d8e4c1a00",
		"{RIDA}":                                 "",
		// An unfilled macro name would merge every such device into one viewer.
		"ROKU_ADS_TRACKING_ID":                 "",
		"RIDA":                                 "",
		"5F1C2E9A-0B3D-4C1E-9A7F-2B6D8E4C1A00": "5F1C2E9A-0B3D-4C1E-9A7F-2B6D8E4C1A00",
		"a b":                                  "",
		"<x>":                                  "",
		strings.Repeat("a", 65):                "",
	} {
		if got := SID(url.Values{"sid": {in}}); got != want {
			t.Errorf("SID(%q) = %q, want %q", in, got, want)
		}
	}
}

func Test_parseQueryIP(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5":      "203.0.113.5",
		" 2001:DB8::1 ":    "2001:db8::1",
		"{RokuIP}":         "",
		"203.0.113.5:8080": "",
		"":                 "",
		// Roku fills its macro with the LAN address; private, loopback,
		// link-local and CGNAT addresses never name a viewer.
		"10.0.0.90":   "",
		"192.168.1.5": "",
		"172.16.0.1":  "",
		"127.0.0.1":   "",
		"169.254.1.1": "",
		"100.64.0.1":  "",
		"0.0.0.0":     "",
		"fd00::1":     "",
		"::1":         "",
	} {
		if got := parseQueryIP(url.Values{"ip": {in}}); got != want {
			t.Errorf("parseQueryIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasher_PrivateIPParamFallsBackToClientIP(t *testing.T) {
	h := NewHasher("salt", false)
	r1 := httptest.NewRequest("GET", "/channels/live.m3u8?ip=10.0.0.90", nil)
	r1.Header.Set("X-Real-IP", "99.178.140.144")
	r2 := httptest.NewRequest("GET", "/channels/live.m3u8", nil)
	r2.Header.Set("X-Real-IP", "99.178.140.144")
	if h.ID(r1) != h.ID(r2) {
		t.Error("a private ip parameter must not replace the client IP")
	}
}

// trusting is a Hasher that believes ip= from the given networks.
func trusting(t *testing.T, cidrs ...string) *Hasher {
	t.Helper()
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		nets = append(nets, n)
	}
	h := NewHasher("salt", false)
	h.TrustIPParamFrom(nets)
	return h
}

func TestHasher_IPParamIsBelievedOnlyFromTrustedAddresses(t *testing.T) {
	req := func(realIP, query string) *http.Request {
		r := httptest.NewRequest("GET", "/channels/live.m3u8"+query, nil)
		r.Header.Set("X-Real-IP", realIP)
		return r
	}
	home := "203.0.113.44"
	for name, h := range map[string]*Hasher{
		"no trusted addresses": NewHasher("salt", false),
		"another network":      trusting(t, "34.1.1.0/24", "2001:db8::/32"),
	} {
		r := req("198.51.100.9", "?ip="+home)
		if got := h.QueryIP(r); got != "" {
			t.Errorf("%s: QueryIP = %q, want it ignored", name, got)
		}
		if got := h.RequestIP(r); got != "198.51.100.9" {
			t.Errorf("%s: RequestIP = %q, want the connecting address", name, got)
		}
		if h.ID(r) == h.ID(req(home, "")) {
			t.Errorf("%s: a stranger naming a home in ip= got that home's viewer id", name)
		}
		if c := h.NewClient(r); c.IP != "198.51.100.9" || h.LimitKey(r) != "198.51.100.9" {
			t.Errorf("%s: client = %+v, limit key %q", name, c, h.LimitKey(r))
		}
	}
	h := trusting(t, "34.1.1.0/24", "198.51.100.9/32")
	for _, from := range []string{"34.1.1.200", "198.51.100.9"} {
		r := req(from, "?ip="+home)
		if h.QueryIP(r) != home || h.RequestIP(r) != home || h.ID(r) != h.ID(req(home, "")) {
			t.Errorf("ip= from trusted %s was not believed", from)
		}
	}
	if h.QueryIP(req("198.51.100.10", "?ip="+home)) != "" {
		t.Error("a bare address in the list must trust only that address")
	}
	if h.QueryIP(req("34.1.1.200", "?ip=10.0.0.90")) != "" {
		t.Error("a private ip= is ignored even from a trusted address")
	}
	if h.trustsIPParam(req("not-an-address", "")) {
		t.Error("an unparsable connecting address is not trusted")
	}
}

func TestNewClient(t *testing.T) {
	r := httptest.NewRequest("GET", "/channels/live.m3u8?ip=10.0.0.90", nil)
	r.Header.Set("X-Real-IP", "99.178.140.144")
	r.Header.Set("User-Agent", "Roku/DVP-15.3 (15.3.4.2402-CU)")
	h := trusting(t, "34.1.1.0/24")
	if c := h.NewClient(r); c.IP != "99.178.140.144" || c.UserAgent != "Roku/DVP-15.3 (15.3.4.2402-CU)" || h.LimitKey(r) != "99.178.140.144" {
		t.Errorf("client = %+v", c)
	}
	r = httptest.NewRequest("GET", "/channels/live.m3u8?ip=198.51.100.7", nil)
	r.Header.Set("X-Real-IP", "34.1.1.1")
	r.Header.Set("User-Agent", strings.Repeat("é", 200)+"\x00")
	c := h.NewClient(r)
	if c.IP != "198.51.100.7" || h.LimitKey(r) != "" {
		t.Errorf("a public ip parameter from a trusted address names the client, and that address is not limited: %+v", c)
	}
	if len(c.UserAgent) > maxUserAgentLen || !utf8.ValidString(c.UserAgent) || strings.Contains(c.UserAgent, "\x00") {
		t.Errorf("user agent not sanitised: len %d", len(c.UserAgent))
	}
}
