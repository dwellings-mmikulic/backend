package linear

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/dwellingtw/backend/internal/viewer"
)

const playlistContentType = "application/vnd.apple.mpegurl"

// Handler serves the channel endpoints.
type Handler struct {
	svc     *Service
	log     *slog.Logger
	viewers *ViewerOptions // nil until EnableViewers

	preRollAd, midRollAd *string // VAST tags for /channels/resolve; nil = none
}

// NewHandler creates a Handler.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// SetAds sets the VAST ad tag URLs returned by /channels/resolve (empty =
// null, no ads in that slot). The Roku app requests them through RAF.
func (h *Handler) SetAds(preRollAd, midRollAd string) {
	h.preRollAd, h.midRollAd = nonEmpty(preRollAd), nonEmpty(midRollAd)
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Register mounts the channel routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /channels/master.m3u8", h.master)
	mux.HandleFunc("GET /channels/live.m3u8", h.live)
	mux.HandleFunc("GET /channels/epg.json", h.epg)
	if h.viewers != nil {
		mux.HandleFunc("GET /channels/resolve", h.resolve)
		mux.HandleFunc("GET /channels/stats", h.stats)
		mux.HandleFunc("GET /channels/beat", h.beat)
	}
}

func (h *Handler) master(w http.ResponseWriter, r *http.Request) {
	sc, err := ParseScope(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	playlistHeaders(w)
	writeMaster(w, mediaURI(sc, r.URL.Query()))
}

// mediaURI is the media playlist reference of sc. It is rebuilt from the
// parsed scope rather than echoing the request's raw query, so unknown or
// duplicated parameters, odd casing and stray whitespace cannot be reflected
// into the playlist body. A valid sid and ip are carried along: the media
// playlist is the request that is tracked, and a player given only a master
// URL (a third-party app) would otherwise drop them.
func mediaURI(sc Scope, q url.Values) string {
	v := scopeValues(sc)
	if sid := viewer.SID(q); sid != "" {
		v.Set("sid", sid)
	}
	if ip := viewer.QueryIP(q); ip != "" {
		v.Set("ip", ip)
	}
	return "live.m3u8" + encodeQuery(v)
}

func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	sc, err := ParseScope(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := h.svc.now()
	body, err := h.svc.Playlist(r.Context(), sc, now)
	if err != nil {
		h.fail(w, sc, err)
		return
	}
	h.track(r, sc, now)
	playlistHeaders(w)
	_, _ = w.Write(body)
}

func (h *Handler) epg(w http.ResponseWriter, r *http.Request) {
	sc, err := ParseScope(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	e, err := h.svc.EPG(r.Context(), sc, h.svc.now())
	if err != nil {
		h.fail(w, sc, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(e)
}

// fail maps service errors to responses.
func (h *Handler) fail(w http.ResponseWriter, sc Scope, err error) {
	switch {
	case errors.Is(err, ErrNoContent):
		writeError(w, http.StatusServiceUnavailable, "no content")
	case errors.Is(err, ErrBadScope):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrUnknownArea):
		writeError(w, http.StatusNotFound, "unknown area")
	default:
		h.log.Error("channel request failed", "channel", sc.Key(), "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func playlistHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", playlistContentType)
	w.Header().Set("Cache-Control", "public, max-age=2")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
