package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/dwellingtw/backend/internal/geo"
	"github.com/dwellingtw/backend/internal/viewer"
)

// Audience is what the resolver reads about viewers (viewer.Store).
type Audience interface {
	LastChannel(ctx context.Context, id viewer.ID, since time.Time) (string, bool, error)
	Stats(ctx context.Context, channel string, now time.Time) (viewer.Stats, error)
}

// Tracker receives a heartbeat per live-playlist request (viewer.Recorder).
type Tracker interface {
	Record(id viewer.ID, channel string, t time.Time)
}

// ViewerOptions enables viewer tracking and the resolve/stats endpoints.
type ViewerOptions struct {
	Hasher        *viewer.Hasher
	Tracker       Tracker    // nil: live requests are not recorded
	Audience      Audience   // required
	Geo           geo.Lookup // nil: no IP geo default
	PublicBaseURL string     // absolute URLs in the resolve response
	// LastWatchedTTL bounds how old a "last watched" channel may be before
	// it is ignored. Zero means 30 days.
	LastWatchedTTL time.Duration
}

// EnableViewers turns on tracking and the /channels/resolve and
// /channels/stats routes. Call before Register.
func (h *Handler) EnableViewers(o ViewerOptions) {
	if o.LastWatchedTTL <= 0 {
		o.LastWatchedTTL = 30 * 24 * time.Hour
	}
	h.viewers = &o
}

// track records a live-playlist request as a heartbeat.
func (h *Handler) track(r *http.Request, sc Scope, now time.Time) {
	if h.viewers == nil || h.viewers.Tracker == nil {
		return
	}
	h.viewers.Tracker.Record(h.viewers.Hasher.ID(r), sc.Key(), now)
}

// resolveResponse is the body of /channels/resolve.
type resolveResponse struct {
	Scope  string `json:"scope"`
	Name   string `json:"name"`
	Master string `json:"master"`
	EPG    string `json:"epg"`
	Source string `json:"source"` // last_watched | geo | default
}

// resolve picks the channel for this viewer: what they watched last, else
// where their IP is, else national. Every candidate goes through the usual
// scope fallback so the answer always has content.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o := h.viewers
	now := h.svc.now()

	try := func(sc Scope) (Scope, bool) {
		eff, _, err := h.svc.resolveScope(ctx, sc)
		if err != nil {
			if !errors.Is(err, ErrNoContent) {
				h.log.Warn("resolve candidate failed", "scope", sc.Key(), "error", err)
			}
			return Scope{}, false
		}
		return eff, true
	}

	id := o.Hasher.ID(r)
	if key, ok, err := o.Audience.LastChannel(ctx, id, now.Add(-o.LastWatchedTTL)); err != nil {
		h.log.Warn("resolve: last channel lookup failed", "error", err)
	} else if ok {
		if sc, err := ParseKey(key); err == nil {
			if eff, ok := try(sc); ok {
				h.writeResolve(w, eff, "last_watched")
				return
			}
		}
	}

	if o.Geo != nil {
		if loc, ok := o.Geo.Locate(net.ParseIP(viewer.ClientIP(r))); ok {
			// A candidate that fell all the way back to national is not a
			// geo answer; a less specific candidate (the city when the ZIP
			// is unknown) may still be.
			for _, sc := range geoCandidates(loc) {
				if eff, ok := try(sc); ok && eff != (Scope{}) {
					h.writeResolve(w, eff, "geo")
					return
				}
			}
		}
	}

	eff, ok := try(Scope{})
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no content")
		return
	}
	h.writeResolve(w, eff, "default")
}

// geoCandidates orders what a location suggests: the ZIP, then the city
// (the ZIP may be unknown to the library while the city is not), then the
// state. Each is validated the way a request filter would be.
func geoCandidates(loc geo.Location) []Scope {
	var out []Scope
	add := func(q url.Values) {
		if sc, err := ParseScope(q); err == nil && sc != (Scope{}) && sc.knownState() {
			out = append(out, sc)
		}
	}
	if loc.Zip != "" {
		add(url.Values{"zip": {loc.Zip}})
	}
	if loc.City != "" && loc.State != "" {
		add(url.Values{"city": {loc.City}, "state": {loc.State}})
	}
	if loc.State != "" {
		add(url.Values{"state": {loc.State}})
	}
	return out
}

func (h *Handler) writeResolve(w http.ResponseWriter, sc Scope, source string) {
	base := h.viewers.PublicBaseURL + "/channels/"
	q := scopeQuery(sc)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(resolveResponse{
		Scope:  sc.Key(),
		Name:   sc.Name(),
		Master: base + "master.m3u8" + q,
		EPG:    base + "epg.json" + q,
		Source: source,
	})
}

// scopeQuery is the canonical query string of sc ("" for national),
// rebuilt from the parsed scope so nothing from a request is reflected.
func scopeQuery(sc Scope) string {
	q := url.Values{}
	switch {
	case sc.Zip != "":
		q.Set("zip", sc.Zip)
	case sc.City != "":
		q.Set("city", sc.City)
		q.Set("state", sc.State)
	case sc.State != "":
		q.Set("state", sc.State)
	default:
		return ""
	}
	return "?" + q.Encode()
}

// beat is the viewer heartbeat for players that do not reach the origin on
// every playlist poll (the playlists are cached at the edge). Apps call it
// about once a minute while playing, with the same filters as the playlist
// they are on and their device id as sid.
func (h *Handler) beat(w http.ResponseWriter, r *http.Request) {
	sc, err := ParseScope(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.track(r, sc, h.svc.now())
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusNoContent)
}

// stats reports a channel's audience.
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	sc, err := ParseScope(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s, err := h.viewers.Audience.Stats(r.Context(), sc.Key(), h.svc.now())
	if err != nil {
		h.log.Error("stats failed", "channel", sc.Key(), "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(struct {
		Scope string `json:"scope"`
		viewer.Stats
	}{sc.Key(), s})
}
