package hls

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Uploader stores an object and returns its public URL (bunny.Client does).
type Uploader interface {
	Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error)
}

// Version is the storage layout version. Bump it if segment naming or cutting
// changes, so lineups that reference old segments keep resolving.
const Version = "v1"

// Prefix is the storage directory of a clip: hls/<Version>/<zpid>/<hash8>.
// Paths are immutable — a re-rendered listing has a new content hash and so a
// new prefix — which lets a lineup already on air keep playing the old clip.
func Prefix(zpid, contentHash string) string {
	h := contentHash
	if len(h) > 8 {
		h = h[:8]
	}
	if h == "" {
		h = "00000000"
	}
	return path.Join("hls", Version, zpid, h)
}

// Upload publishes every segment in dir plus its index playlist under prefix,
// with at most concurrency uploads in flight. It returns the clip's base URL
// (the index playlist's URL without "/index.m3u8"); segment i is then
// base + "/" + SegmentName(i).
func Upload(ctx context.Context, up Uploader, dir, prefix string, clip Clip, concurrency int) (string, error) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(concurrency, 1))
	for i := range clip.SegmentMS {
		name := SegmentName(i)
		g.Go(func() error {
			f, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := up.Upload(gctx, path.Join(prefix, name), f, "video/MP2T"); err != nil {
				return fmt.Errorf("upload %s: %w", name, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Join(dir, IndexName))
	if err != nil {
		return "", err
	}
	defer f.Close()
	url, err := up.Upload(ctx, path.Join(prefix, IndexName), f, "application/vnd.apple.mpegurl")
	if err != nil {
		return "", fmt.Errorf("upload %s: %w", IndexName, err)
	}
	return strings.TrimSuffix(url, "/"+IndexName), nil
}
