package linear

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dwellingtw/backend/internal/music"
)

// Channel music: one loop that plays straight through every listing. A
// playlist rendered with music on lists every segment through
// /channels/m/…, where the segment is fetched from the CDN and its sound
// swapped for the loop's (see package music). The player receives the same
// kind of segment as without music, picture and sound together.

// MusicOptions configures the channel music.
type MusicOptions struct {
	Loop *music.Loop
	// Default is whether playlists carry the music when the request does not
	// say (music=1 or music=0 in the query always decides).
	Default bool
	// Platforms are the platform paths (/channels/<platform>/…) whose
	// playlists carry the music when the request does not say, whatever
	// Default is.
	Platforms []string
	// CDNBaseURL is where the segments live, without a trailing slash.
	// Segments elsewhere are listed as they are.
	CDNBaseURL string
	// Client fetches segments from the CDN; nil for a default one.
	Client *http.Client
}

// EnableMusic turns on the channel music. Call before Register.
func (h *Handler) EnableMusic(o MusicOptions) {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 20 * time.Second}
	}
	o.CDNBaseURL = strings.TrimRight(o.CDNBaseURL, "/")
	h.music = &o
}

const (
	musicSegmentPrefix = "/channels/m/"
	// musicSegmentMaxBytes bounds a segment read from the CDN. Listing
	// segments are around 1 MB.
	musicSegmentMaxBytes = 64 << 20
	// musicSegmentTTL is how long a served segment may be cached. A segment
	// is only on a playlist for under a minute.
	musicSegmentTTL = 10 * time.Minute
	// musicSegmentWindow is how far from the clock a segment's airing time
	// may be. Playlists only list segments that ended within the last minute
	// or so. Without this every airing time ever would be a segment to fetch
	// and swap.
	musicSegmentWindow = 10 * time.Minute
)

// musicSegmentPath is the only shape of CDN path the segment route fetches.
var musicSegmentPath = regexp.MustCompile(`^hls/[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*/seg-[0-9]+\.ts$`)

type musicKey struct{}

// musicBase is the CDN base whose segments are listed through the music
// route for a playlist rendered with ctx; "" when the music is off.
func musicBase(ctx context.Context) string {
	base, _ := ctx.Value(musicKey{}).(string)
	return base
}

// musicParam reads a request's music=1 / music=0; ok is false when it has
// neither.
func musicParam(q url.Values) (on, ok bool) {
	switch q.Get("music") {
	case "1":
		return true, true
	case "0":
		return false, true
	}
	return false, false
}

// withMusic returns r with its context marked when its playlist should
// carry the music: the request's own music= parameter, or the default for
// the path it came in on.
func (h *Handler) withMusic(r *http.Request) *http.Request {
	if h.music == nil {
		return r
	}
	on, ok := musicParam(r.URL.Query())
	if !ok {
		on = h.music.Default || slices.Contains(h.music.Platforms, r.PathValue("platform"))
	}
	if !on {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), musicKey{}, h.music.CDNBaseURL))
}

// carryMusic copies a request's music= parameter into v, so a master
// requested with it hands it on to its media playlist.
func carryMusic(v, q url.Values) {
	if on, ok := musicParam(q); ok {
		v.Set("music", map[bool]string{true: "1", false: "0"}[on])
	}
}

// renderPlaylist writes segs as a live playlist, through the music route
// when ctx asks for it.
func renderPlaylist(ctx context.Context, w io.Writer, segs []segment) {
	if base := musicBase(ctx); base != "" {
		segs = musicSegments(segs, base)
	}
	writePlaylist(w, segs)
}

// musicSegments is segs with every segment under base listed through the
// music route. The URL carries what the swap needs and nothing else: when
// the segment airs, how long it is and how far into its clip it starts.
func musicSegments(segs []segment, base string) []segment {
	out := make([]segment, len(segs))
	for i, s := range segs {
		if path, ok := strings.CutPrefix(s.URL, base+"/"); ok && musicSegmentPath.MatchString(path) {
			s.URL = fmt.Sprintf("%s%d-%d-%d/%s", musicSegmentPrefix, s.Start.UnixMilli(), s.DurMS, s.ElapsedMS, path)
		}
		out[i] = s
	}
	return out
}

func (h *Handler) registerMusic(mux *http.ServeMux) {
	mux.HandleFunc("GET "+musicSegmentPrefix+"{spec}/{path...}", h.musicSegment)
}

// musicSegment serves one video segment with the music in place of its own
// sound. A segment the music cannot be put into is served as it is: a
// stretch of the old sound is better than a hole in the picture.
//
// The timings in the address are checked before anything is fetched: they
// come from whoever sends the request, and the work grows with them.
func (h *Handler) musicSegment(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	parts := strings.Split(r.PathValue("spec"), "-")
	if len(parts) != 3 || !musicSegmentPath.MatchString(path) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	start, err1 := strconv.ParseInt(parts[0], 10, 64)
	dur, err2 := strconv.Atoi(parts[1])
	elapsed, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || !music.ValidTiming(start, dur, elapsed) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if off := h.svc.now().Sub(time.UnixMilli(start)); off < -musicSegmentWindow || off > musicSegmentWindow {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	video, status, err := h.fetchSegment(r.Context(), h.music.CDNBaseURL+"/"+path)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			h.log.Error("music segment fetch failed", "path", path, "error", err)
		}
		writeError(w, status, "segment unavailable")
		return
	}
	body, err := h.music.Loop.Remux(video, start, dur, elapsed)
	if err != nil {
		h.log.Warn("music segment served without music", "path", path, "spec", r.PathValue("spec"), "error", err)
		body = video
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(musicSegmentTTL.Seconds())))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(body)
}

// fetchSegment reads a segment from the CDN. On failure status is what to
// answer the player with.
func (h *Handler) fetchSegment(ctx context.Context, u string) (body []byte, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	resp, err := h.music.Client.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, http.StatusNotFound, fmt.Errorf("CDN answered 404 for %s", u)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, http.StatusBadGateway, fmt.Errorf("CDN answered %d for %s", resp.StatusCode, u)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, musicSegmentMaxBytes+1))
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	if len(body) > musicSegmentMaxBytes {
		return nil, http.StatusBadGateway, fmt.Errorf("segment %s is over %d bytes", u, musicSegmentMaxBytes)
	}
	return body, http.StatusOK, nil
}
