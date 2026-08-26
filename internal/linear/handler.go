package linear

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
)

const playlistContentType = "application/vnd.apple.mpegurl"

// Handler serves the channel endpoints.
type Handler struct {
	svc *Service
	log *slog.Logger
}

// NewHandler creates a Handler.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Register mounts the channel routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /channels/master.m3u8", h.master)
	mux.HandleFunc("GET /channels/live.m3u8", h.live)
	mux.HandleFunc("GET /channels/epg.json", h.epg)
}

func (h *Handler) master(w http.ResponseWriter, r *http.Request) {
	sc, err := ParseScope(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	playlistHeaders(w)
	writeMaster(w, mediaURI(sc))
}

// mediaURI is the media playlist reference of sc. It is rebuilt from the
// parsed scope rather than echoing the request's raw query, so unknown or
// duplicated parameters, odd casing and stray whitespace cannot be reflected
// into the playlist body.
func mediaURI(sc Scope) string {
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
		return "live.m3u8"
	}
	return "live.m3u8?" + q.Encode()
}

func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	sc, err := ParseScope(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := h.svc.Playlist(r.Context(), sc, h.svc.now())
	if err != nil {
		h.fail(w, sc, err)
		return
	}
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
