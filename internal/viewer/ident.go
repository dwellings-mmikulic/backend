// Package viewer identifies who is watching the linear channels without
// storing anything that names them: a salted hash of a session id or of the
// client IP, recorded once per minute per channel.
// See docs/superpowers/specs/2026-08-26-viewer-tracking-design.md.
package viewer

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
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

// ID identifies the request's viewer: by the sid query parameter when
// present, else by client IP.
func (h *Hasher) ID(r *http.Request) ID {
	return h.IDAt(strings.TrimSpace(r.URL.Query().Get("sid")), ClientIP(r), h.now())
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
