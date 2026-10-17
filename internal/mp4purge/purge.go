// Package mp4purge retires a listing's MP4 from the CDN once the same render
// is served as HLS.
//
// Every listing video is uploaded twice: the MP4 (videos/<zpid>.mp4) and a
// remux of the very same bytes into TS segments with a VOD playlist
// (hls/v1/<zpid>/<hash8>/index.m3u8). The Roku feed and the API hand out the
// playlist whenever it exists, so the MP4 copy costs storage for nothing. An
// MP4 can always be rebuilt from the segments without re-encoding
// (ffmpeg -i index.m3u8 -c copy), so nothing is lost by deleting it.
//
// Order of work per listing: the playlist is fetched from the CDN and must be
// a complete VOD playlist, so the only remaining copy is proven playable
// before the MP4 goes; then the object is deleted; then video_url is swapped
// to the playlist URL. The swap is conditional on the row still holding the
// MP4 URL and the content hash it was read with: a listing re-rendered in
// between keeps its new state, and is ErrVideoChanged to the caller. A crash
// between the delete and the swap leaves a row still naming the MP4, which is
// what the next run finds and finishes: deleting an object that is already
// gone is a success.
package mp4purge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dwellingtw/backend/internal/property"
)

// Storage deletes objects by path and knows which CDN URLs are its own
// (bunny.Client).
type Storage interface {
	Delete(ctx context.Context, path string) error
	ObjectPath(cdnURL string) (string, error)
}

// Store swaps a listing's video URL (property.Repository). SwapVideoURL
// fails with property.ErrVideoChanged when the row no longer holds from
// under contentHash.
type Store interface {
	SwapVideoURL(ctx context.Context, zpid, from, to, contentHash string) error
}

// Prober confirms that an HLS playlist is really served before the MP4 it
// replaces is deleted.
type Prober interface {
	Playable(ctx context.Context, playlistURL string) error
}

// Purger retires MP4s. It is safe for concurrent use.
type Purger struct {
	storage Storage
	store   Store
	probe   Prober
	log     *slog.Logger
}

// New returns a Purger.
func New(storage Storage, store Store, probe Prober, log *slog.Logger) *Purger {
	return &Purger{storage: storage, store: store, probe: probe, log: log}
}

// Check reports whether Purge would touch the listing: the MP4 must be this
// listing's own object (videos/<zpid>.mp4) and the playlist this listing's own
// clip (hls/v1/<zpid>/.../index.m3u8), both on our CDN. A dry run uses it to
// report what a real run would do.
//
// video_url holds the listing's MP4 by convention only; a row pointing
// anywhere else (a manual fix, a URL already swapped by hand, another
// listing's file) is left alone rather than deleted on the strength of the
// convention.
func (p *Purger) Check(s property.VideoSwap) error {
	_, err := p.mp4Path(s)
	return err
}

// Purge deletes the listing's MP4 and points its row at the playlist.
func (p *Purger) Purge(ctx context.Context, s property.VideoSwap) error {
	path, err := p.mp4Path(s)
	if err != nil {
		return err
	}
	if err := p.probe.Playable(ctx, s.HLSURL); err != nil {
		return fmt.Errorf("mp4 zpid=%s kept, its HLS copy is not playable: %w", s.ZPID, err)
	}
	if err := p.storage.Delete(ctx, path); err != nil {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	if err := p.store.SwapVideoURL(ctx, s.ZPID, s.MP4URL, s.HLSURL, s.ContentHash); err != nil {
		return fmt.Errorf("swap video url zpid=%s: %w", s.ZPID, err)
	}
	p.log.Debug("mp4 purged", "zpid", s.ZPID, "deleted", path, "now", s.HLSURL)
	return nil
}

// mp4Path maps the swap to the MP4's storage path, refusing what Check refuses.
func (p *Purger) mp4Path(s property.VideoSwap) (string, error) {
	path, err := p.storage.ObjectPath(s.MP4URL)
	if err != nil {
		return "", fmt.Errorf("mp4 zpid=%s left alone: %w", s.ZPID, err)
	}
	if path != "videos/"+s.ZPID+".mp4" {
		return "", fmt.Errorf("mp4 zpid=%s left alone: %q is not its MP4", s.ZPID, s.MP4URL)
	}
	hls, err := p.storage.ObjectPath(s.HLSURL)
	if err != nil {
		return "", fmt.Errorf("mp4 zpid=%s left alone: %w", s.ZPID, err)
	}
	if !strings.HasPrefix(hls, "hls/v1/"+s.ZPID+"/") || !strings.HasSuffix(hls, "/index.m3u8") {
		return "", fmt.Errorf("mp4 zpid=%s left alone: %q is not its HLS playlist", s.ZPID, s.HLSURL)
	}
	return path, nil
}

// HTTPProber fetches a playlist from the CDN and accepts a complete VOD
// playlist: one that starts with #EXTM3U, names at least one segment and
// ends with #EXT-X-ENDLIST. Anything else, a 404 included, is an error.
type HTTPProber struct {
	Client *http.Client
}

// maxPlaylist bounds what Playable reads: a listing's playlist is a few KB.
const maxPlaylist = 1 << 20

// Playable implements Prober.
func (h HTTPProber) Playable(ctx context.Context, playlistURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURL, nil)
	if err != nil {
		return fmt.Errorf("build playlist request: %w", err)
	}
	res, err := h.Client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch playlist: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch playlist: status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxPlaylist))
	if err != nil {
		return fmt.Errorf("read playlist: %w", err)
	}
	return checkPlaylist(string(body))
}

// ErrNotVOD is a playlist that is not a complete VOD playlist.
var ErrNotVOD = errors.New("not a complete VOD playlist")

func checkPlaylist(text string) error {
	switch {
	case !strings.HasPrefix(text, "#EXTM3U"):
		return fmt.Errorf("%w: no #EXTM3U header", ErrNotVOD)
	case !strings.Contains(text, "#EXTINF:"):
		return fmt.Errorf("%w: no segments", ErrNotVOD)
	case !strings.Contains(text, "#EXT-X-ENDLIST"):
		return fmt.Errorf("%w: no #EXT-X-ENDLIST", ErrNotVOD)
	}
	return nil
}
