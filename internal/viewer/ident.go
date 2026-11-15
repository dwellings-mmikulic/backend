// Package viewer identifies who is watching the linear channels: a salted
// hash of a session id or of the client IP, recorded once per minute per
// channel, plus the raw client address and user agent behind those
// heartbeats for the key-protected /admin/viewers listing.
// See docs/superpowers/specs/2026-08-26-viewer-tracking-design.md.
package viewer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ID is a pseudonymous viewer identifier: the first 16 bytes of a salted
// SHA-256. It is the only thing that is ever stored.
type ID [16]byte

// String is the hex form.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Hasher derives IDs.
type Hasher struct {
	salt        string
	rotateDaily bool
	now         func() time.Time
	trusted     []*net.IPNet // connecting addresses whose ip= is believed
}

// NewHasher creates a Hasher. With rotateDaily, the UTC date is mixed into
// the hash so an id cannot be linked across days.
func NewHasher(salt string, rotateDaily bool) *Hasher {
	return &Hasher{salt: salt, rotateDaily: rotateDaily, now: time.Now}
}

// TrustIPParamFrom sets the connecting addresses whose ip= query parameter
// is believed: the servers of a platform that fetches playlists on its
// viewers' behalf. Call before serving. With none set, ip= is ignored and
// every viewer is the address the request came from.
func (h *Hasher) TrustIPParamFrom(nets []*net.IPNet) { h.trusted = nets }

// trustsIPParam reports whether r comes from an address whose ip= is
// believed.
func (h *Hasher) trustsIPParam(r *http.Request) bool {
	if len(h.trusted) == 0 {
		return false
	}
	ip := net.ParseIP(ClientIP(r))
	if ip == nil {
		return false
	}
	for _, network := range h.trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// ID identifies the request's viewer: by a valid sid query parameter, else
// by RequestIP.
func (h *Hasher) ID(r *http.Request) ID {
	return h.IDAt(SID(r.URL.Query()), h.RequestIP(r), h.now())
}

// maxSIDLen caps a session id; a Roku RIDA is a 36-character UUID.
const maxSIDLen = 64

// SID is the sid query parameter when it looks like a device or session id
// (1–64 of A–Z a–z 0–9 . _ : -), else "". A platform macro the player did
// not fill is not one: neither {RIDA} nor a bare macro name such as
// ROKU_ADS_TRACKING_ID (only capitals and underscores), which would merge
// every device that sent it into one viewer. Playlists echo it, so it must
// not be able to carry anything else.
func SID(q url.Values) string {
	s := strings.TrimSpace(q.Get("sid"))
	if s == "" || len(s) > maxSIDLen {
		return ""
	}
	macroName := true
	for _, c := range s {
		upper := c >= 'A' && c <= 'Z' || c == '_'
		ok := upper || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '.' || c == ':' || c == '-'
		if !ok {
			return ""
		}
		macroName = macroName && upper
	}
	if macroName {
		return ""
	}
	return s
}

// parseQueryIP is the ip query parameter in canonical form when it is a
// public IP address, else "". It only reads the value: whether to believe it
// is Hasher.QueryIP's decision. An unfilled macro is not an address, and a
// private address is ignored: Roku fills its macro with the device's LAN
// address (10.0.0.90), which thousands of homes share and which would hide
// the public address the request really came from.
func parseQueryIP(query url.Values) string {
	return PublicIP(query.Get(ipParam))
}

// QueryIP is the viewer address a platform passed in ip= (e.g.
// ip={RokuIP}), or "". The value decides whose feed is served and whose
// views are recorded, so it is believed only from the addresses set with
// TrustIPParamFrom. From anyone else it would let a client pass for any
// home it can name.
func (h *Hasher) QueryIP(r *http.Request) string {
	if !h.trustsIPParam(r) {
		return ""
	}
	return parseQueryIP(r.URL.Query())
}

// PublicIP is s as a canonical public address, or "" when s is not an
// address or not a public one (private, loopback, link-local, CGNAT).
func PublicIP(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || !isPublic(ip) {
		return ""
	}
	return ip.String()
}

// cgnat is 100.64.0.0/10, carrier-grade NAT space, which net.IP.IsPrivate
// does not cover.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isPublic(ip net.IP) bool {
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsInterfaceLocalMulticast() && !ip.IsMulticast() && !cgnat.Contains(ip)
}

// RequestIP is the viewer's address: QueryIP when set, else ClientIP.
func (h *Hasher) RequestIP(r *http.Request) string {
	if ip := h.QueryIP(r); ip != "" {
		return ip
	}
	return ClientIP(r)
}

// IDAt is ID with explicit inputs. sid wins over ip when non-empty; the two
// live in separate namespaces so a sid can never collide with an ip.
func (h *Hasher) IDAt(sid, ip string, t time.Time) ID {
	kind, value := "ip", ip
	if sid != "" {
		kind, value = "sid", sid
	}
	sum := sha256.New()
	sum.Write([]byte(h.salt))
	if h.rotateDaily {
		sum.Write([]byte{0})
		sum.Write([]byte(t.UTC().Format("2006-01-02")))
	}
	sum.Write([]byte{0})
	sum.Write([]byte(kind))
	sum.Write([]byte{0})
	sum.Write([]byte(value))
	var id ID
	copy(id[:], sum.Sum(nil))
	return id
}

// Household identifies the home a request comes from: the ip-kind hash of
// its public address, whatever sid it carries. A TV and a phone on the same
// home network share it, which is how the mobile page finds the TV's feed.
func (h *Hasher) Household(ip string) ID { return h.IDAt("", ip, h.now()) }

// ParseID reads the hex form ID.String produces.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 2*len(id) {
		return id, fmt.Errorf("viewer: id must be %d hex characters", 2*len(id))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("viewer: bad id: %w", err)
	}
	copy(id[:], b)
	return id, nil
}

// ipParam is the query parameter a platform names the viewer's address in.
const ipParam = "ip"

// maxUserAgentLen caps the stored user agent.
const maxUserAgentLen = 256

// One customer holds a whole IPv6 /64, so the heartbeat limit counts by it.
const (
	ipv6CustomerPrefixBits = 64
	ipv6Bits               = 8 * net.IPv6len
)

// LimitKey is what the recorder limits r's heartbeat rows by: the address it
// came from, or "" (no limit) for a platform whose ip= is believed, since
// its servers speak for many homes. An IPv6 address counts as its /64: one
// customer holds a whole /64 and could otherwise use a new address per row.
func (h *Hasher) LimitKey(r *http.Request) string {
	if h.trustsIPParam(r) {
		return ""
	}
	from := ClientIP(r)
	if ip := net.ParseIP(from); ip != nil && ip.To4() == nil {
		customerMask := net.CIDRMask(ipv6CustomerPrefixBits, ipv6Bits)
		return (&net.IPNet{IP: ip.Mask(customerMask), Mask: customerMask}).String()
	}
	return from
}

// NewClient is the raw client behind r: RequestIP and the User-Agent,
// trimmed to maxUserAgentLen and made safe for a PostgreSQL text column.
func (h *Hasher) NewClient(r *http.Request) Client {
	ua := strings.TrimSpace(r.UserAgent())
	if len(ua) > maxUserAgentLen {
		ua = ua[:maxUserAgentLen]
	}
	ua = strings.ToValidUTF8(strings.ReplaceAll(ua, "\x00", ""), "")
	return Client{IP: h.RequestIP(r), UserAgent: ua}
}

// ClientIP is the address the request came from as seen by the edge:
// X-Real-IP (set by nginx), else the first X-Forwarded-For hop, else the
// connection's remote address without its port.
func ClientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		first, _, _ := strings.Cut(v, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
