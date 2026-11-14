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
	hasher := trusting(t, "198.51.100.1/32")
	req := func(target, realIP string) ID {
		r := httptest.NewRequest("GET", target, nil)
		r.Header.Set("X-Real-IP", realIP)
		return hasher.ID(r)
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
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		nets = append(nets, network)
	}
	hasher := NewHasher("salt", false)
	hasher.TrustIPParamFrom(nets)
	return hasher
}

func TestHasher_IPParamIsBelievedOnlyFromTrustedAddresses(t *testing.T) {
	req := func(realIP, query string) *http.Request {
		request := httptest.NewRequest("GET", "/channels/live.m3u8"+query, nil)
		request.Header.Set("X-Real-IP", realIP)
		return request
	}
	home := "203.0.113.44"
	for name, hasher := range map[string]*Hasher{
		"no trusted addresses": NewHasher("salt", false),
		"another network":      trusting(t, "34.1.1.0/24", "2001:db8::/32"),
	} {
		request := req("198.51.100.9", "?ip="+home)
		if got := hasher.QueryIP(request); got != "" {
			t.Errorf("%s: QueryIP = %q, want it ignored", name, got)
		}
		if got := hasher.RequestIP(request); got != "198.51.100.9" {
			t.Errorf("%s: RequestIP = %q, want the connecting address", name, got)
		}
		if hasher.ID(request) == hasher.ID(req(home, "")) {
			t.Errorf("%s: a stranger naming a home in ip= got that home's viewer id", name)
		}
		if client := hasher.NewClient(request); client.IP != "198.51.100.9" || hasher.LimitKey(request) != "198.51.100.9" {
			t.Errorf("%s: client = %+v, limit key %q", name, client, hasher.LimitKey(request))
		}
	}
	trusted := trusting(t, "34.1.1.0/24", "198.51.100.9/32")
	for _, from := range []string{"34.1.1.200", "198.51.100.9"} {
		request := req(from, "?ip="+home)
		if trusted.QueryIP(request) != home || trusted.RequestIP(request) != home || trusted.ID(request) != trusted.ID(req(home, "")) {
			t.Errorf("ip= from trusted %s was not believed", from)
		}
	}
	if trusted.QueryIP(req("198.51.100.10", "?ip="+home)) != "" {
		t.Error("a bare address in the list must trust only that address")
	}
	if trusted.QueryIP(req("34.1.1.200", "?ip=10.0.0.90")) != "" {
		t.Error("a private ip= is ignored even from a trusted address")
	}
	if trusted.trustsIPParam(req("not-an-address", "")) {
		t.Error("an unparsable connecting address is not trusted")
	}
}

func TestHasher_LimitKeyCountsIPv6ByItsNetwork(t *testing.T) {
	hasher := NewHasher("salt", false)
	key := func(realIP string) string {
		request := httptest.NewRequest("GET", "/channels/beat", nil)
		request.Header.Set("X-Real-IP", realIP)
		return hasher.LimitKey(request)
	}
	// Every address of one customer's /64 is one key, another /64 is another.
	if first, second := key("2001:db8:1:2::1"), key("2001:db8:1:2:ffff:ffff:ffff:ffff"); first != second || first != "2001:db8:1:2::/64" {
		t.Errorf("same /64: %q and %q", first, second)
	}
	if key("2001:db8:1:3::1") == key("2001:db8:1:2::1") {
		t.Error("different /64s share a key")
	}
	// IPv4, also when written as an IPv6-mapped address, is the address.
	if got := key("203.0.113.5"); got != "203.0.113.5" {
		t.Errorf("IPv4 key = %q", got)
	}
	if got := key("::ffff:203.0.113.5"); got != "::ffff:203.0.113.5" {
		t.Errorf("mapped IPv4 key = %q", got)
	}
	if got := key("not-an-address"); got != "not-an-address" {
		t.Errorf("unparsable key = %q", got)
	}
}

func TestNewClient(t *testing.T) {
	request := httptest.NewRequest("GET", "/channels/live.m3u8?ip=10.0.0.90", nil)
	request.Header.Set("X-Real-IP", "99.178.140.144")
	request.Header.Set("User-Agent", "Roku/DVP-15.3 (15.3.4.2402-CU)")
	hasher := trusting(t, "34.1.1.0/24")
	if client := hasher.NewClient(request); client.IP != "99.178.140.144" || client.UserAgent != "Roku/DVP-15.3 (15.3.4.2402-CU)" || hasher.LimitKey(request) != "99.178.140.144" {
		t.Errorf("client = %+v", client)
	}
	request = httptest.NewRequest("GET", "/channels/live.m3u8?ip=198.51.100.7", nil)
	request.Header.Set("X-Real-IP", "34.1.1.1")
	request.Header.Set("User-Agent", strings.Repeat("é", 200)+"\x00")
	client := hasher.NewClient(request)
	if client.IP != "198.51.100.7" || hasher.LimitKey(request) != "" {
		t.Errorf("a public ip parameter from a trusted address names the client, and that address is not limited: %+v", client)
	}
	if len(client.UserAgent) > maxUserAgentLen || !utf8.ValidString(client.UserAgent) || strings.Contains(client.UserAgent, "\x00") {
		t.Errorf("user agent not sanitised: len %d", len(client.UserAgent))
	}
}
