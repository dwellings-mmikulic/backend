package linear

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
	if _, err := ParseScope(r.URL.Query()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	uri := "live.m3u8"
	if r.URL.RawQuery != "" {
		uri += "?" + r.URL.RawQuery
	}
	playlistHeaders(w)
	writeMaster(w, uri)
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
