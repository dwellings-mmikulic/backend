package linear

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

const (
	defaultAdminHours = 24
	maxAdminHours     = 30 * 24
	defaultAdminLimit = 500
	maxAdminLimit     = 5000
)

// adminViewers lists the raw client addresses behind the heartbeats, most
// recently seen first. It is outside /channels/ so the edge never caches it,
// and it answers only to the admin key, as a bearer token or ?key=.
//
//	GET /admin/viewers?hours=24&limit=500
func (h *Handler) adminViewers(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	hours, ok := boundedInt(q.Get("hours"), defaultAdminHours, maxAdminHours)
	if !ok {
		writeError(w, http.StatusBadRequest, "hours must be 1–720")
		return
	}
	limit, ok := boundedInt(q.Get("limit"), defaultAdminLimit, maxAdminLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be 1–5000")
		return
	}
	since := h.svc.now().Add(-time.Duration(hours) * time.Hour)
	clients, err := h.viewers.Clients.Clients(r.Context(), since, limit)
	if err != nil {
		h.log.Error("admin viewers failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if clients == nil {
		clients = []viewer.ClientSeen{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Since   time.Time           `json:"since"`
		Count   int                 `json:"count"`
		Viewers []viewer.ClientSeen `json:"viewers"`
	}{since, len(clients), clients})
}

func (h *Handler) adminAuthorized(r *http.Request) bool {
	key := r.URL.Query().Get("key")
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		key = bearer
	}
	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(h.viewers.AdminKey)) == 1
}

// boundedInt parses s as 1..max, defaulting when s is empty.
func boundedInt(s string, def, max int) (int, bool) {
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}
