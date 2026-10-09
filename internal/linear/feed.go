package linear

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/dwellingtw/backend/internal/qrcode"
	"github.com/dwellingtw/backend/internal/viewer"
)

// FeedOptions enables the personal feeds (/feed/*) and the mobile page
// (/tv/*). Requires EnableViewers: feeds are keyed by the viewer hash.
type FeedOptions struct {
	MobileBaseURL  string // origin of the mobile page, for the QR (https://dwellings.tv)
	QRShowSeconds  int    // how long the Roku app shows the QR
	QREverySeconds int    // how often
}

// EnableFeeds turns on the personal feeds. Call before Register.
func (h *Handler) EnableFeeds(o FeedOptions) { h.feeds = &o }

func (h *Handler) registerFeeds(mux *http.ServeMux) {
	mux.HandleFunc("GET /feed/master.m3u8", h.feedMaster)
	mux.HandleFunc("GET /feed/live.m3u8", h.feedLive)
	mux.HandleFunc("GET /feed/qr.png", h.feedQR)
	mux.HandleFunc("GET /feed/me", h.feedMe)
	mux.HandleFunc("GET /feed/areas", h.feedAreas)
	mux.HandleFunc("GET /feed/{id}", h.feedGet)
	mux.HandleFunc("POST /feed/{id}", h.feedChoose)
	mux.HandleFunc("OPTIONS /feed/{id}", h.feedPreflight)
	mux.HandleFunc("GET /tv/", h.tvPage)
	mux.HandleFunc("GET /tv/{id}", h.tvPage)
}

const (
	qrDefaultSize = 400
	qrMinSize     = 100
	qrMaxSize     = 1000
	maxChoiceBody = 1024
)

// household is the home the request comes from.
func (h *Handler) household(r *http.Request) viewer.ID {
	return h.viewers.Hasher.Household(viewer.RequestIP(r))
}

// ensure creates the household's feed on first contact, defaulting to the
// IP's area when the geo database is present.
func (h *Handler) ensure(r *http.Request, id viewer.ID) ([]Span, error) {
	var cands []Scope
	if h.viewers.Geo != nil {
		if loc, ok := h.viewers.Geo.Locate(net.ParseIP(viewer.RequestIP(r))); ok {
			cands = geoCandidates(loc)
		}
	}
	return h.svc.EnsureHousehold(r.Context(), id, cands, h.svc.now())
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// failFeed maps feed errors to responses.
func (h *Handler) failFeed(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnknownHousehold) {
		writeError(w, http.StatusNotFound, "unknown feed")
		return
	}
	h.fail(w, Scope{}, err)
}

func parseHousehold(s string) (viewer.ID, bool) {
	id, err := viewer.ParseID(s)
	return id, err == nil
}

func (h *Handler) feedMaster(w http.ResponseWriter, r *http.Request) {
	id := h.household(r)
	if _, err := h.ensure(r, id); err != nil {
		h.failFeed(w, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", playlistContentType)
	writeMaster(w, "live.m3u8?hh="+id.String())
}

func (h *Handler) feedLive(w http.ResponseWriter, r *http.Request) {
	id, ok := parseHousehold(r.URL.Query().Get("hh"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid hh")
		return
	}
	now := h.svc.now()
	body, airing, err := h.svc.PersonalPlaylist(r.Context(), id, now)
	if err != nil {
		h.failFeed(w, err)
		return
	}
	if sc, err := ParseKey(airing.Scope); err == nil {
		h.track(r, sc, now)
	}
	playlistHeaders(w)
	_, _ = w.Write(body)
}

func (h *Handler) feedQR(w http.ResponseWriter, r *http.Request) {
	id := h.household(r)
	if _, err := h.ensure(r, id); err != nil {
		h.failFeed(w, err)
		return
	}
	size := qrDefaultSize
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < qrMinSize || n > qrMaxSize {
			writeError(w, http.StatusBadRequest, "invalid size (100-1000)")
			return
		}
		size = n
	}
	png, err := qrcode.PNG(h.feeds.MobileBaseURL+"/tv/"+id.String(), size)
	if err != nil {
		h.log.Error("qr render failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

// feedResponse is the body of GET/POST /feed/{id} and GET /feed/me.
type feedResponse struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`     // effective channel key
	Name      string    `json:"name"`      // its viewer-facing title
	Requested string    `json:"requested"` // what the viewer asked for ("" = nothing yet)
	Source    string    `json:"source"`    // choice | geo | default
	UpdatedAt time.Time `json:"updated_at"`
	Live      string    `json:"live"` // the personal media playlist, for an in-page player
}

func (h *Handler) writeFeed(w http.ResponseWriter, spans []Span) {
	sp := spans[len(spans)-1]
	name := ""
	if sc, err := ParseKey(sp.Scope); err == nil {
		name = sc.Name()
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(feedResponse{
		ID: sp.Household.String(), Scope: sp.Scope, Name: name, Requested: sp.Requested, Source: sp.Source,
		UpdatedAt: sp.CreatedAt, Live: h.viewers.PublicBaseURL + "/feed/live.m3u8?hh=" + sp.Household.String(),
	})
}

func (h *Handler) feedMe(w http.ResponseWriter, r *http.Request) {
	spans, err := h.ensure(r, h.household(r))
	if err != nil {
		h.failFeed(w, err)
		return
	}
	h.writeFeed(w, spans)
}

func (h *Handler) feedGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseHousehold(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid feed id")
		return
	}
	spans, err := h.svc.Household(r.Context(), id)
	if err != nil {
		h.failFeed(w, err)
		return
	}
	h.writeFeed(w, spans)
}

// choiceBody is what the mobile page posts: a ZIP, or a city and state.
type choiceBody struct {
	Zip   string `json:"zip"`
	City  string `json:"city"`
	State string `json:"state"`
}

func (h *Handler) feedChoose(w http.ResponseWriter, r *http.Request) {
	id, ok := parseHousehold(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid feed id")
		return
	}
	var body choiceBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxChoiceBody+1)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with zip, or city and state")
		return
	}
	sc, err := ParseScope(url.Values{"zip": {body.Zip}, "city": {body.City}, "state": {body.State}})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	spans, err := h.svc.Choose(r.Context(), id, sc, h.svc.now())
	if err != nil {
		h.failFeed(w, err)
		return
	}
	h.writeFeed(w, spans)
}

func (h *Handler) feedPreflight(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// areaResponse is one entry of GET /feed/areas.
type areaResponse struct {
	City  string `json:"city"`
	State string `json:"state"`
	Name  string `json:"name"`
	Clips int    `json:"clips"`
}

func (h *Handler) feedAreas(w http.ResponseWriter, r *http.Request) {
	cs, err := h.svc.Cities(r.Context())
	if err != nil {
		h.fail(w, Scope{}, err)
		return
	}
	out := make([]areaResponse, 0, len(cs))
	for _, c := range cs {
		out = append(out, areaResponse{City: c.City, State: c.State, Name: Scope{City: c.City, State: c.State}.areaName(), Clips: c.Clips})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) tvPage(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not yet", http.StatusNotFound)
}
