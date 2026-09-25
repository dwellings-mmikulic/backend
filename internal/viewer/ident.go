// Package viewer identifies who is watching the linear channels: a salted
// hash of a session id or of the client IP, recorded once per minute per
// channel, plus the raw client address and user agent behind those
// heartbeats for the key-protected /admin/viewers listing.
// See docs/superpowers/specs/2026-08-26-viewer-tracking-design.md.
package viewer

import (
	"crypto/sha256"
	"encoding/hex"
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
}

// NewHasher creates a Hasher. With rotateDaily, the UTC date is mixed into
// the hash so an id cannot be linked across days.
func NewHasher(salt string, rotateDaily bool) *Hasher {
	return &Hasher{salt: salt, rotateDaily: rotateDaily, now: time.Now}
}

// ID identifies the request's viewer: by a valid sid query parameter, else
// by a valid ip query parameter, else by client IP.
func (h *Hasher) ID(r *http.Request) ID {
	return h.IDAt(SID(r.URL.Query()), RequestIP(r), h.now())
}

// maxSIDLen caps a session id; a Roku RIDA is a 36-character UUID.
const maxSIDLen = 64

// SID is the sid query parameter when it looks like a device or session id
// (1–64 of A–Z a–z 0–9 . _ : -), else "". A platform macro the player did
// not fill, such as {RIDA}, is not one. Playlists echo it, so it must not be
// able to carry anything else.
func SID(q url.Values) string {
	s := strings.TrimSpace(q.Get("sid"))
	if s == "" || len(s) > maxSIDLen {
		return ""
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == ':' || c == '-'
		if !ok {
			return ""
		}
	}
	return s
}

// QueryIP is the ip query parameter in canonical form when it is a public
// IP address, else "". Streaming platforms fill it through a macro (e.g.
// ip={RokuIP}) so the viewer is known even when a server of theirs, not the
// device, makes the request. An unfilled macro is ignored, and so is a
// private address: Roku fills its macro with the device's LAN address
// (10.0.0.90), which thousands of homes share and which would hide the
// public address the request really came from.
func QueryIP(q url.Values) string {
	ip := net.ParseIP(strings.TrimSpace(q.Get("ip")))
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
func RequestIP(r *http.Request) string {
	if ip := QueryIP(r.URL.Query()); ip != "" {
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

// maxUserAgentLen caps the stored user agent.
const maxUserAgentLen = 256

// NewClient is the raw client behind r: RequestIP and the User-Agent,
// trimmed to maxUserAgentLen and made safe for a PostgreSQL text column.
func NewClient(r *http.Request) Client {
	ua := strings.TrimSpace(r.UserAgent())
	if len(ua) > maxUserAgentLen {
		ua = ua[:maxUserAgentLen]
	}
	ua = strings.ToValidUTF8(strings.ReplaceAll(ua, "\x00", ""), "")
	return Client{IP: RequestIP(r), UserAgent: ua}
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
