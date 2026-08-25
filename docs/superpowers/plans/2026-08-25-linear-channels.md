# Linear Channels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve the listing-video library as 24/7 linear HLS channels (national, or filtered by state / city / ZIP with fallback) plus an EPG, without a running encoder.

**Architecture:** Each MP4 is remuxed once into TS segments on Bunny CDN (`internal/hls`). A channel is a deterministic chain of DB-materialised lineup versions; the live playlist is computed per request as a pure function of `(channel, wall-clock)` (`internal/linear`). The scheduler segments new renders; `cmd/backfill-hls` segments the backlog; the HTTP server mounts the channel endpoints and the Roku feed gains a `liveFeeds` entry.

**Tech Stack:** Go 1.26, ffmpeg 6.1 (`-c copy` remux, hls muxer), PostgreSQL via pgx v5, Bunny Storage, `golang.org/x/sync/errgroup`. No new module dependencies.

**Spec:** `docs/superpowers/specs/2026-08-25-linear-channels-design.md`

## Global Constraints

- Go `1.26.4` (go.mod); `max`/`min` builtins are available. No new dependencies.
- Module path `github.com/dwellingtw/backend`. Packages live under `internal/`.
- `TargetDuration = 10` s, `WindowSegments = 6`, storage layout version `"v1"`, prefix `hls/v1/<zpid>/<hash8>/`.
- Channel keys: `us`, `state:tx`, `city:katy|tx`, `zip:77494` (lowercase).
- Defaults: `LINEAR_ENABLED=true`, `LINEAR_LINEUP_HOURS=6`, `LINEAR_MIN_SCOPE_CLIPS=20`, `LINEAR_EPG_HORIZON_HOURS=24`, `PUBLIC_BASE_URL=""`.
- Playlist headers: `Content-Type: application/vnd.apple.mpegurl`, `Cache-Control: public, max-age=2`, `Access-Control-Allow-Origin: *`. EPG: `application/json`, `public, max-age=300`.
- Errors are JSON `{"error": "..."}` like `internal/api`.
- Every step that runs tests: `go test ./internal/<pkg>/ -run <Name> -v` from the repo root; run `gofmt -l .` and `go vet ./...` before each commit (both must print nothing).
- Commit messages follow the repo style: `feat: …`, `test: …`, `docs: …`; end with the Co-Authored-By / Claude-Session trailer used by the previous commits (see `git log -1 --format=%B`).
- Never `cd` out of the worktree; never use bare `git stash`.

---

### Task 1: `internal/hls` — remux a clip into TS segments

**Files:**
- Create: `internal/hls/hls.go`
- Create: `internal/hls/hls_test.go`
- Create: `internal/hls/segment_integration_test.go`

**Interfaces:**
- Produces: `hls.Clip{SegmentMS []int; TotalMS int}`, `hls.TargetDuration = 10`, `hls.ErrSegmentTooLong`, `hls.SegmentName(i int) string` (`seg-000.ts`), `hls.IndexName = "index.m3u8"`, `hls.Args(mp4Path string) []string`, `hls.NewSegmenter() *Segmenter`, `(*Segmenter).Segment(ctx, mp4Path, outDir string) (Clip, error)`, `hls.ParsePlaylist(io.Reader) (Clip, error)`.

- [ ] **Step 1: Write the failing tests**

`internal/hls/hls_test.go`:

```go
package hls

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestArgs_RemuxesAtEveryKeyframe(t *testing.T) {
	got := Args("/work/video.mp4")
	want := []string{
		"-y", "-i", "/work/video.mp4",
		"-map", "0:v:0", "-map", "0:a:0",
		"-c", "copy",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", "seg-%03d.ts",
		"index.m3u8",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Args =\n%v\nwant\n%v", got, want)
	}
}

func TestSegmentName(t *testing.T) {
	if got := SegmentName(7); got != "seg-007.ts" {
		t.Errorf("SegmentName(7) = %q", got)
	}
}

func TestParsePlaylist_ReadsDurations(t *testing.T) {
	src := `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:9
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-INDEPENDENT-SEGMENTS
#EXTINF:3.000000,
seg-000.ts
#EXTINF:8.333333,
seg-001.ts
#EXTINF:2.666667,
seg-002.ts
#EXT-X-ENDLIST
`
	c, err := ParsePlaylist(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{3000, 8333, 2667}; !reflect.DeepEqual(c.SegmentMS, want) {
		t.Errorf("SegmentMS = %v, want %v", c.SegmentMS, want)
	}
	if c.TotalMS != 14000 {
		t.Errorf("TotalMS = %d, want 14000", c.TotalMS)
	}
}

func TestParsePlaylist_RejectsSegmentOverTargetDuration(t *testing.T) {
	src := "#EXTM3U\n#EXTINF:10.001,\nseg-000.ts\n"
	_, err := ParsePlaylist(strings.NewReader(src))
	if !errors.Is(err, ErrSegmentTooLong) {
		t.Fatalf("err = %v, want ErrSegmentTooLong", err)
	}
}

func TestParsePlaylist_RejectsEmpty(t *testing.T) {
	if _, err := ParsePlaylist(strings.NewReader("#EXTM3U\n")); err == nil {
		t.Fatal("expected error for playlist without segments")
	}
}
```

`internal/hls/segment_integration_test.go`:

```go
package hls

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSegment_RemuxesGeneratedClip needs ffmpeg with libx264 + aac on PATH
// (the Docker image has it; Homebrew ffmpeg does too). It is skipped otherwise.
func TestSegment_RemuxesGeneratedClip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mp4")
	// 7 s of a test pattern + silent audio, encoded like the listing videos
	// (30 fps, x264, aac) with a forced keyframe every 3 s.
	gen := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=30",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-t", "7",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "ultrafast",
		"-force_key_frames", "expr:gte(t,n_forced*3)",
		"-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clip: %v: %s", err, out)
	}

	outDir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clip, err := NewSegmenter().Segment(context.Background(), src, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(clip.SegmentMS) < 2 {
		t.Fatalf("expected at least 2 segments (keyframes every 3 s over 7 s), got %v", clip.SegmentMS)
	}
	if clip.TotalMS < 6500 || clip.TotalMS > 7500 {
		t.Errorf("TotalMS = %d, want ~7000", clip.TotalMS)
	}
	for i := range clip.SegmentMS {
		if _, err := os.Stat(filepath.Join(outDir, SegmentName(i))); err != nil {
			t.Errorf("segment %d missing: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, IndexName)); err != nil {
		t.Errorf("index missing: %v", err)
	}
}

func TestSegment_FailsWithoutAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "silent.mp4")
	gen := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=30",
		"-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "ultrafast", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clip: %v: %s", err, out)
	}
	if _, err := NewSegmenter().Segment(context.Background(), src, dir); err == nil {
		t.Fatal("expected error for a clip without an audio track")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/hls/ -v`
Expected: build failure — `undefined: Args`, `SegmentName`, `ParsePlaylist`, `NewSegmenter`.

- [ ] **Step 3: Write the implementation**

`internal/hls/hls.go`:

```go
// Package hls cuts listing MP4s into HTTP Live Streaming segments. The cut is
// a remux (-c copy) at existing keyframes: every listing video shares one
// encode profile, so no transcoding is needed for the segments to chain
// inside a linear channel.
package hls

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// TargetDuration is the channel-wide EXT-X-TARGETDURATION in seconds. A clip
// with a longer segment cannot be aired and is rejected at segmentation. The
// listing encoder caps keyframe spacing at 250 frames (8.33 s at 30 fps), so
// remuxed segments always fit.
const TargetDuration = 10

// IndexName is the per-clip VOD playlist written next to the segments.
const IndexName = "index.m3u8"

// ErrSegmentTooLong is returned when a clip has a segment longer than
// TargetDuration.
var ErrSegmentTooLong = errors.New("segment longer than target duration")

// Clip is the segment layout of one video.
type Clip struct {
	SegmentMS []int // duration of each segment in milliseconds, in order
	TotalMS   int
}

// SegmentName is the file name of segment i within a clip directory.
func SegmentName(i int) string { return fmt.Sprintf("seg-%03d.ts", i) }

// Args builds the ffmpeg argument list that remuxes mp4Path into TS segments
// in the working directory. Output names are relative on purpose: ffmpeg
// writes them verbatim into the index playlist, and the segmenter runs ffmpeg
// with its working directory set to the output directory. Pure, so tests can
// assert it without running ffmpeg.
func Args(mp4Path string) []string {
	return []string{
		"-y", "-i", mp4Path,
		"-map", "0:v:0", "-map", "0:a:0",
		"-c", "copy",
		"-f", "hls",
		"-hls_time", "2", // keyframes are >= 2 s apart, so this cuts at every one
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", "seg-%03d.ts",
		IndexName,
	}
}

// Segmenter runs ffmpeg.
type Segmenter struct{ ffmpeg string }

// NewSegmenter returns a Segmenter using the ffmpeg binary on PATH.
func NewSegmenter() *Segmenter { return &Segmenter{ffmpeg: "ffmpeg"} }

// Segment cuts mp4Path into outDir (which must exist) and returns the parsed
// clip layout. A clip without an audio track fails (-map 0:a:0 has nothing to
// map), as does one with a segment longer than TargetDuration.
func (s *Segmenter) Segment(ctx context.Context, mp4Path, outDir string) (Clip, error) {
	abs, err := filepath.Abs(mp4Path)
	if err != nil {
		return Clip{}, err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.ffmpeg, Args(abs)...)
	cmd.Dir = outDir
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Clip{}, fmt.Errorf("ffmpeg segment %s: %w: %s", mp4Path, err, tail(stderr.String(), 600))
	}
	f, err := os.Open(filepath.Join(outDir, IndexName))
	if err != nil {
		return Clip{}, fmt.Errorf("open segment index: %w", err)
	}
	defer f.Close()
	return ParsePlaylist(f)
}

// ParsePlaylist reads the EXTINF durations of a VOD playlist into a Clip.
func ParsePlaylist(r io.Reader) (Clip, error) {
	var c Clip
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		v := strings.TrimPrefix(line, "#EXTINF:")
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return Clip{}, fmt.Errorf("parse %q: %w", line, err)
		}
		ms := int(math.Round(secs * 1000))
		if ms > TargetDuration*1000 {
			return Clip{}, fmt.Errorf("%w: %.3fs", ErrSegmentTooLong, secs)
		}
		c.SegmentMS = append(c.SegmentMS, ms)
		c.TotalMS += ms
	}
	if err := sc.Err(); err != nil {
		return Clip{}, err
	}
	if len(c.SegmentMS) == 0 {
		return Clip{}, errors.New("playlist has no segments")
	}
	return c, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/hls/ -v`
Expected: all PASS (the two integration tests may SKIP on a machine without ffmpeg; they must PASS on one with it — verify locally, Homebrew ffmpeg is present).

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/hls/
git add internal/hls/
git commit -m "feat: hls segmenter remuxes listing videos into TS segments"
```

---

### Task 2: `internal/hls` — publish a clip's segments to the CDN

**Files:**
- Create: `internal/hls/upload.go`
- Create: `internal/hls/upload_test.go`

**Interfaces:**
- Consumes: `hls.Clip`, `hls.SegmentName`, `hls.IndexName` (Task 1).
- Produces: `hls.Uploader` interface (`Upload(ctx, path string, content io.Reader, contentType string) (string, error)` — satisfied by `*bunny.Client`), `hls.Version = "v1"`, `hls.Prefix(zpid, contentHash string) string`, `hls.Upload(ctx, up Uploader, dir, prefix string, clip Clip, concurrency int) (baseURL string, err error)`.

- [ ] **Step 1: Write the failing test**

`internal/hls/upload_test.go`:

```go
package hls

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

type fakeUploader struct {
	mu    sync.Mutex
	paths map[string]string // path → content type
}

func (f *fakeUploader) Upload(_ context.Context, path string, content io.Reader, contentType string) (string, error) {
	_, _ = io.Copy(io.Discard, content)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paths == nil {
		f.paths = map[string]string{}
	}
	f.paths[path] = contentType
	return "https://cdn.example/" + path, nil
}

func TestPrefix(t *testing.T) {
	if got := Prefix("123", "abcdef0123456789"); got != "hls/v1/123/abcdef01" {
		t.Errorf("Prefix = %q", got)
	}
	if got := Prefix("123", ""); got != "hls/v1/123/00000000" {
		t.Errorf("Prefix with empty hash = %q", got)
	}
}

func TestUpload_PublishesSegmentsAndIndex(t *testing.T) {
	dir := t.TempDir()
	clip := Clip{SegmentMS: []int{3000, 3000, 2000}, TotalMS: 8000}
	for i := range clip.SegmentMS {
		if err := os.WriteFile(filepath.Join(dir, SegmentName(i)), []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, IndexName), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	up := &fakeUploader{}
	base, err := Upload(context.Background(), up, dir, "hls/v1/123/abcdef01", clip, 2)
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://cdn.example/hls/v1/123/abcdef01" {
		t.Errorf("base = %q", base)
	}
	var got []string
	for p := range up.paths {
		got = append(got, p)
	}
	sort.Strings(got)
	want := []string{
		"hls/v1/123/abcdef01/index.m3u8",
		"hls/v1/123/abcdef01/seg-000.ts",
		"hls/v1/123/abcdef01/seg-001.ts",
		"hls/v1/123/abcdef01/seg-002.ts",
	}
	if len(got) != len(want) {
		t.Fatalf("uploaded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("uploaded[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if ct := up.paths["hls/v1/123/abcdef01/seg-000.ts"]; ct != "video/MP2T" {
		t.Errorf("segment content type = %q", ct)
	}
	if ct := up.paths["hls/v1/123/abcdef01/index.m3u8"]; ct != "application/vnd.apple.mpegurl" {
		t.Errorf("index content type = %q", ct)
	}
}

func TestUpload_FailsWhenSegmentMissing(t *testing.T) {
	dir := t.TempDir()
	clip := Clip{SegmentMS: []int{3000}, TotalMS: 3000}
	if _, err := Upload(context.Background(), &fakeUploader{}, dir, "hls/v1/1/0", clip, 1); err == nil {
		t.Fatal("expected error when a segment file is missing")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/hls/ -run 'TestPrefix|TestUpload' -v`
Expected: build failure — `undefined: Prefix`, `Upload`.

- [ ] **Step 3: Write the implementation**

`internal/hls/upload.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/hls/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/hls/
git add internal/hls/upload.go internal/hls/upload_test.go
git commit -m "feat: publish hls segments to the CDN under immutable prefixes"
```

---

### Task 3: schema + channel scope parsing

**Files:**
- Modify: `internal/db/schema.sql` (append at end)
- Create: `internal/linear/scope.go`
- Create: `internal/linear/scope_test.go`

**Interfaces:**
- Produces: `linear.Scope{Zip, City, State string}` (zero value = national), `linear.ErrBadScope`, `linear.ParseScope(url.Values) (Scope, error)`, `(Scope).Key() string`, `linear.ParseKey(string) (Scope, error)`, `(Scope).Name() string`.

- [ ] **Step 1: Append the tables to the schema**

Append to `internal/db/schema.sql`:

```sql

-- Linear channels (see docs/superpowers/specs/2026-08-25-linear-channels-design.md).
-- video_hls: one row per (listing, render); segments live at base_url on the
-- CDN. Rows are never deleted so lineups that reference an old render keep
-- resolving. A row is "current" when properties.video_content_hash matches.
CREATE TABLE IF NOT EXISTS video_hls (
    id           BIGSERIAL PRIMARY KEY,
    zpid         TEXT NOT NULL REFERENCES properties(zpid),
    content_hash TEXT NOT NULL,
    base_url     TEXT NOT NULL,
    segment_ms   INTEGER[] NOT NULL,
    total_ms     INTEGER NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (zpid, content_hash)
);

-- channel_lineups: the deterministic air schedule of a channel, as a chain of
-- versions. Version N starts where N-1 ended and continues its counters.
CREATE TABLE IF NOT EXISTS channel_lineups (
    channel_key TEXT NOT NULL,
    version     INTEGER NOT NULL,
    scope       TEXT NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL,
    start_seq   BIGINT NOT NULL,
    start_item  BIGINT NOT NULL,
    item_ids    BIGINT[] NOT NULL,
    item_ms     INTEGER[] NOT NULL,
    item_segs   INTEGER[] NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_key, version)
);
```

- [ ] **Step 2: Write the failing tests**

`internal/linear/scope_test.go`:

```go
package linear

import (
	"errors"
	"net/url"
	"testing"
)

func TestParseScope_Keys(t *testing.T) {
	tests := []struct {
		query string
		key   string
		name  string
	}{
		{"", "us", "Homes for sale across the US"},
		{"state=TX", "state:tx", "Homes for sale in Texas"},
		{"city=Katy&state=tx", "city:katy|tx", "Homes for sale in Katy, TX"},
		{"city=San%20%20Antonio&state=TX", "city:san antonio|tx", "Homes for sale in San Antonio, TX"},
		{"zip=77494", "zip:77494", "Homes for sale in 77494"},
	}
	for _, tt := range tests {
		q, _ := url.ParseQuery(tt.query)
		s, err := ParseScope(q)
		if err != nil {
			t.Errorf("%q: %v", tt.query, err)
			continue
		}
		if s.Key() != tt.key {
			t.Errorf("%q: key = %q, want %q", tt.query, s.Key(), tt.key)
		}
		if s.Name() != tt.name {
			t.Errorf("%q: name = %q, want %q", tt.query, s.Name(), tt.name)
		}
		back, err := ParseKey(s.Key())
		if err != nil || back != s {
			t.Errorf("%q: ParseKey(%q) = %+v, %v; want %+v", tt.query, s.Key(), back, err, s)
		}
	}
}

func TestParseScope_Rejects(t *testing.T) {
	for _, q := range []string{"city=Katy", "zip=77494&state=tx", "zip=7749", "zip=abcde", "state=tex"} {
		v, _ := url.ParseQuery(q)
		if _, err := ParseScope(v); !errors.Is(err, ErrBadScope) {
			t.Errorf("%q: err = %v, want ErrBadScope", q, err)
		}
	}
}

func TestParseKey_Rejects(t *testing.T) {
	for _, k := range []string{"", "city:katy", "planet:earth"} {
		if _, err := ParseKey(k); !errors.Is(err, ErrBadScope) {
			t.Errorf("%q: err = %v, want ErrBadScope", k, err)
		}
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/linear/ -v`
Expected: build failure — package has no Go files / `undefined: ParseScope`.

- [ ] **Step 4: Write the implementation**

`internal/linear/scope.go`:

```go
// Package linear serves the listing-video library as 24/7 linear HLS
// channels: a deterministic lineup per channel, materialised in the database
// in versions, and a live playlist computed per request from the wall clock.
// See docs/superpowers/specs/2026-08-25-linear-channels-design.md.
package linear

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Scope is a geographic slice of the library. The zero value is national.
// City and State are lowercase; Zip is five digits.
type Scope struct {
	Zip   string
	City  string
	State string
}

// ErrBadScope reports an invalid channel filter.
var ErrBadScope = errors.New("invalid channel filter")

// ParseScope reads zip, city+state or state from query parameters. It is
// strict about combinations so that every distinct URL maps to one key.
func ParseScope(q url.Values) (Scope, error) {
	s := Scope{
		Zip:   strings.TrimSpace(q.Get("zip")),
		City:  strings.ToLower(strings.Join(strings.Fields(q.Get("city")), " ")),
		State: strings.ToLower(strings.TrimSpace(q.Get("state"))),
	}
	switch {
	case s.Zip != "" && (s.City != "" || s.State != ""):
		return Scope{}, fmt.Errorf("%w: zip cannot be combined with city or state", ErrBadScope)
	case s.City != "" && s.State == "":
		return Scope{}, fmt.Errorf("%w: city requires state", ErrBadScope)
	case s.Zip != "" && !isZip(s.Zip):
		return Scope{}, fmt.Errorf("%w: zip must be 5 digits", ErrBadScope)
	case s.State != "" && len(s.State) != 2:
		return Scope{}, fmt.Errorf("%w: state must be a 2-letter code", ErrBadScope)
	}
	return s, nil
}

func isZip(z string) bool {
	if len(z) != 5 {
		return false
	}
	for _, r := range z {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Key is the canonical channel identifier: "us", "state:tx", "city:katy|tx"
// or "zip:77494". It is the channel_lineups.channel_key.
func (s Scope) Key() string {
	switch {
	case s.Zip != "":
		return "zip:" + s.Zip
	case s.City != "":
		return "city:" + s.City + "|" + s.State
	case s.State != "":
		return "state:" + s.State
	}
	return "us"
}

// ParseKey inverts Key. It accepts only what Key produces.
func ParseKey(k string) (Scope, error) {
	switch {
	case k == "us":
		return Scope{}, nil
	case strings.HasPrefix(k, "zip:"):
		return Scope{Zip: k[len("zip:"):]}, nil
	case strings.HasPrefix(k, "state:"):
		return Scope{State: k[len("state:"):]}, nil
	case strings.HasPrefix(k, "city:"):
		city, state, ok := strings.Cut(k[len("city:"):], "|")
		if !ok {
			return Scope{}, fmt.Errorf("%w: %q", ErrBadScope, k)
		}
		return Scope{City: city, State: state}, nil
	}
	return Scope{}, fmt.Errorf("%w: %q", ErrBadScope, k)
}

// Name is the viewer-facing channel title.
func (s Scope) Name() string {
	switch {
	case s.Zip != "":
		return "Homes for sale in " + s.Zip
	case s.City != "":
		return "Homes for sale in " + titleCase(s.City) + ", " + strings.ToUpper(s.State)
	case s.State != "":
		if n, ok := stateNames[s.State]; ok {
			return "Homes for sale in " + n
		}
		return "Homes for sale in " + strings.ToUpper(s.State)
	}
	return "Homes for sale across the US"
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

var stateNames = map[string]string{
	"al": "Alabama", "ak": "Alaska", "az": "Arizona", "ar": "Arkansas", "ca": "California",
	"co": "Colorado", "ct": "Connecticut", "de": "Delaware", "dc": "Washington, DC", "fl": "Florida",
	"ga": "Georgia", "hi": "Hawaii", "id": "Idaho", "il": "Illinois", "in": "Indiana",
	"ia": "Iowa", "ks": "Kansas", "ky": "Kentucky", "la": "Louisiana", "me": "Maine",
	"md": "Maryland", "ma": "Massachusetts", "mi": "Michigan", "mn": "Minnesota", "ms": "Mississippi",
	"mo": "Missouri", "mt": "Montana", "ne": "Nebraska", "nv": "Nevada", "nh": "New Hampshire",
	"nj": "New Jersey", "nm": "New Mexico", "ny": "New York", "nc": "North Carolina", "nd": "North Dakota",
	"oh": "Ohio", "ok": "Oklahoma", "or": "Oregon", "pa": "Pennsylvania", "ri": "Rhode Island",
	"sc": "South Carolina", "sd": "South Dakota", "tn": "Tennessee", "tx": "Texas", "ut": "Utah",
	"vt": "Vermont", "va": "Virginia", "wa": "Washington", "wv": "West Virginia", "wi": "Wisconsin",
	"wy": "Wyoming",
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/linear/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./internal/linear/ ./internal/db/
git add internal/db/schema.sql internal/linear/
git commit -m "feat: linear channel schema and scope parsing"
```

---

### Task 4: lineup chain — store interface, deterministic versions, fallback

**Files:**
- Create: `internal/linear/store.go`
- Create: `internal/linear/lineup.go`
- Create: `internal/linear/service.go` (struct + constructor only; playlist/EPG methods come in Tasks 6–7)
- Create: `internal/linear/store_test.go` (in-memory fake used by every later test)
- Create: `internal/linear/lineup_test.go`

**Interfaces:**
- Consumes: `Scope`, `ParseKey` (Task 3).
- Produces: `linear.Store` interface, `linear.ClipRef`, `linear.ClipSegments`, `linear.Listing`, `linear.Version` (+ `TotalMS() int`, `Segments() int64`), `linear.ErrNoContent`, `linear.Options{LineupHours, MinScopeClips, EPGHorizonHours int}`, `linear.New(store Store, opts Options, log *slog.Logger) *Service`, unexported `(*Service).versionAt(ctx, key string, t time.Time) (cur, prev *Version, err error)`, `buildItems`, `seedFor`, `(*Service).resolveScope`. Test helper `newMemStore()` with `addClip(id int64, sc Scope, price int64, segMS ...int)`.

- [ ] **Step 1: Write the store interface and the in-memory fake**

`internal/linear/store.go`:

```go
package linear

import (
	"context"
	"time"
)

// ClipRef is what lineup building needs to know about a clip.
type ClipRef struct {
	ID       int64
	TotalMS  int
	Segments int
}

// ClipSegments is what playlist generation needs about a clip.
type ClipSegments struct {
	ID        int64
	BaseURL   string
	SegmentMS []int
}

// Listing is what the EPG needs about the property behind a clip.
type Listing struct {
	ClipID int64
	ZPID   string
	Price  int64
	City   string
	State  string
}

// Version is one materialised lineup of a channel. Versions form a chain:
// version N starts at N-1's EndsAt and continues its segment and item
// counters, so playlist sequence numbers stay monotonic across the chain.
type Version struct {
	Key       string
	Version   int
	Scope     string // effective scope key after fallback
	StartsAt  time.Time
	EndsAt    time.Time
	StartSeq  int64   // media sequence number of the first segment
	StartItem int64   // global index of item 0 (0 only for version 1)
	ItemIDs   []int64 // video_hls ids in air order
	ItemMS    []int
	ItemSegs  []int
}

// TotalMS is the version's air time.
func (v *Version) TotalMS() int {
	t := 0
	for _, ms := range v.ItemMS {
		t += ms
	}
	return t
}

// Segments is the number of segments across all items.
func (v *Version) Segments() int64 {
	var n int64
	for _, s := range v.ItemSegs {
		n += int64(s)
	}
	return n
}

// Store is the persistence the channel service needs.
type Store interface {
	// ListClips returns the current clips in scope, ordered by id.
	ListClips(ctx context.Context, s Scope) ([]ClipRef, error)
	// CityOfZip returns the lowercase city and state most listings in zip
	// belong to; both empty when the ZIP is unknown.
	CityOfZip(ctx context.Context, zip string) (city, state string, err error)
	// LatestVersions returns up to n versions of key, newest first.
	LatestVersions(ctx context.Context, key string, n int) ([]Version, error)
	// ListVersions returns every version of key, oldest first.
	ListVersions(ctx context.Context, key string) ([]Version, error)
	// InsertVersion stores v unless (key, version) already exists; ok reports
	// whether v was stored.
	InsertVersion(ctx context.Context, v *Version) (ok bool, err error)
	// ClipsByID returns the segment layout of each id.
	ClipsByID(ctx context.Context, ids []int64) (map[int64]ClipSegments, error)
	// ListingsByClipID returns the listing behind each clip id.
	ListingsByClipID(ctx context.Context, ids []int64) ([]Listing, error)
}
```

`internal/linear/store_test.go`:

```go
package linear

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// memStore is an in-memory Store for tests.
type memStore struct {
	mu       sync.Mutex
	clips    map[int64]memClip
	versions map[string][]Version
	zipCity  map[string][2]string
}

type memClip struct {
	seg   ClipSegments
	scope Scope // where the listing is: Zip, City, State all set
	price int64
}

func newMemStore() *memStore {
	return &memStore{clips: map[int64]memClip{}, versions: map[string][]Version{}, zipCity: map[string][2]string{}}
}

// addClip registers clip id in scope sc (Zip, City and State should all be
// set) with the given segment durations.
func (m *memStore) addClip(id int64, sc Scope, price int64, segMS ...int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clips[id] = memClip{
		seg:   ClipSegments{ID: id, BaseURL: fmt.Sprintf("https://cdn/hls/v1/%d/x", id), SegmentMS: segMS},
		scope: sc, price: price,
	}
	if sc.Zip != "" {
		m.zipCity[sc.Zip] = [2]string{sc.City, sc.State}
	}
}

func matches(want, have Scope) bool {
	switch {
	case want.Zip != "":
		return have.Zip == want.Zip
	case want.City != "":
		return have.City == want.City && have.State == want.State
	case want.State != "":
		return have.State == want.State
	}
	return true
}

func (m *memStore) ListClips(_ context.Context, s Scope) ([]ClipRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ClipRef
	for id, c := range m.clips {
		if !matches(s, c.scope) {
			continue
		}
		total := 0
		for _, ms := range c.seg.SegmentMS {
			total += ms
		}
		out = append(out, ClipRef{ID: id, TotalMS: total, Segments: len(c.seg.SegmentMS)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) CityOfZip(_ context.Context, zip string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.zipCity[zip]
	return cs[0], cs[1], nil
}

func (m *memStore) LatestVersions(_ context.Context, key string, n int) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := append([]Version(nil), m.versions[key]...)
	sort.Slice(vs, func(i, j int) bool { return vs[i].Version > vs[j].Version })
	if len(vs) > n {
		vs = vs[:n]
	}
	return vs, nil
}

func (m *memStore) ListVersions(_ context.Context, key string) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := append([]Version(nil), m.versions[key]...)
	sort.Slice(vs, func(i, j int) bool { return vs[i].Version < vs[j].Version })
	return vs, nil
}

func (m *memStore) InsertVersion(_ context.Context, v *Version) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.versions[v.Key] {
		if e.Version == v.Version {
			return false, nil
		}
	}
	m.versions[v.Key] = append(m.versions[v.Key], *v)
	return true, nil
}

func (m *memStore) ClipsByID(_ context.Context, ids []int64) (map[int64]ClipSegments, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int64]ClipSegments{}
	for _, id := range ids {
		if c, ok := m.clips[id]; ok {
			out[id] = c.seg
		}
	}
	return out, nil
}

func (m *memStore) ListingsByClipID(_ context.Context, ids []int64) ([]Listing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Listing
	for _, id := range ids {
		if c, ok := m.clips[id]; ok {
			out = append(out, Listing{ClipID: id, ZPID: fmt.Sprint(id), Price: c.price, City: c.scope.City, State: c.scope.State})
		}
	}
	return out, nil
}
```

- [ ] **Step 2: Write the failing tests**

`internal/linear/lineup_test.go`:

```go
package linear

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func testService(store Store, now time.Time) *Service {
	s := New(store, Options{LineupHours: 6, MinScopeClips: 3, EPGHorizonHours: 24}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }
	return s
}

var katy = Scope{Zip: "77494", City: "katy", State: "tx"}
var austin = Scope{Zip: "78701", City: "austin", State: "tx"}

// addClips adds n one-minute clips (20 segments of 3 s) starting at id.
func addClips(m *memStore, sc Scope, firstID int64, n int) {
	for i := int64(0); i < int64(n); i++ {
		segs := make([]int, 20)
		for j := range segs {
			segs[j] = 3000
		}
		m.addClip(firstID+i, sc, 300000+i*1000, segs...)
	}
}

func TestBuildItems_DeterministicAndCapped(t *testing.T) {
	var clips []ClipRef
	for id := int64(1); id <= 10; id++ {
		clips = append(clips, ClipRef{ID: id, TotalMS: 60000, Segments: 20})
	}
	ids1, ms, segs := buildItems(clips, seedFor("us", 1), 5*60000)
	ids2, _, _ := buildItems(clips, seedFor("us", 1), 5*60000)
	if !reflect.DeepEqual(ids1, ids2) {
		t.Errorf("same seed gave different orders: %v vs %v", ids1, ids2)
	}
	if len(ids1) != 5 {
		t.Errorf("cap: got %d items, want 5", len(ids1))
	}
	for i := range ids1 {
		if ms[i] != 60000 || segs[i] != 20 {
			t.Errorf("item %d: ms=%d segs=%d", i, ms[i], segs[i])
		}
	}
	// A clip longer than the cap still airs (a lineup is never empty).
	ids, _, _ := buildItems([]ClipRef{{ID: 9, TotalMS: 99999999, Segments: 1}}, 1, 1000)
	if len(ids) != 1 {
		t.Errorf("oversized single clip: got %d items, want 1", len(ids))
	}
	// Input order does not matter; ids are canonicalised before shuffling.
	rev := make([]ClipRef, len(clips))
	for i, c := range clips {
		rev[len(clips)-1-i] = c
	}
	ids3, _, _ := buildItems(rev, seedFor("us", 1), 5*60000)
	if !reflect.DeepEqual(ids1, ids3) {
		t.Errorf("reversed input gave different order: %v vs %v", ids1, ids3)
	}
}

func TestResolveScope_FallsBackUpTheHierarchy(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 2)    // ZIP 77494 / Katy: 2 clips (< MinScopeClips 3)
	addClips(m, austin, 100, 5) // Austin: 5 clips
	s := testService(m, t0)

	// ZIP → city has only 2 → state has 7 → resolves to state:tx.
	eff, clips, err := s.resolveScope(context.Background(), Scope{Zip: "77494"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Key() != "state:tx" || len(clips) != 7 {
		t.Errorf("zip 77494 → %s with %d clips, want state:tx with 7", eff.Key(), len(clips))
	}
	// Austin has enough on its own.
	eff, clips, _ = s.resolveScope(context.Background(), Scope{City: "austin", State: "tx"})
	if eff.Key() != "city:austin|tx" || len(clips) != 5 {
		t.Errorf("austin → %s with %d clips", eff.Key(), len(clips))
	}
	// Unknown ZIP → national.
	eff, _, _ = s.resolveScope(context.Background(), Scope{Zip: "00000"})
	if eff.Key() != "us" {
		t.Errorf("unknown zip → %s, want us", eff.Key())
	}
	// Another state with nothing → national.
	eff, _, _ = s.resolveScope(context.Background(), Scope{State: "ca"})
	if eff.Key() != "us" {
		t.Errorf("empty state → %s, want us", eff.Key())
	}
	// Empty library → ErrNoContent.
	if _, _, err := testService(newMemStore(), t0).resolveScope(context.Background(), Scope{}); !errors.Is(err, ErrNoContent) {
		t.Errorf("empty library: err = %v, want ErrNoContent", err)
	}
}

func TestVersionAt_FirstVersionStartsAnHourBack(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 200) // 200 min of content > 1 h, so v1 covers now
	s := testService(m, t0.Add(37*time.Second))

	cur, prev, err := s.versionAt(context.Background(), "zip:77494", t0.Add(37*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil {
		t.Errorf("prev = %+v, want nil for version 1", prev)
	}
	if cur.Version != 1 || cur.StartSeq != 0 || cur.StartItem != 0 {
		t.Errorf("v1 counters: %+v", cur)
	}
	if want := t0.Add(-time.Hour); !cur.StartsAt.Equal(want) {
		t.Errorf("StartsAt = %s, want %s (now − 1 h, minute-truncated)", cur.StartsAt, want)
	}
	if cur.Scope != "zip:77494" {
		t.Errorf("scope = %s", cur.Scope)
	}
	if cur.TotalMS() > 6*3600*1000 {
		t.Errorf("version exceeds LineupHours: %d ms", cur.TotalMS())
	}
}

func TestVersionAt_ChainsVersionsUntilCovered(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 3) // 3 min per version; an hour of history needs ~20 versions
	s := testService(m, t0)

	cur, prev, err := s.versionAt(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !(cur.StartsAt.Before(t0) || cur.StartsAt.Equal(t0)) || !t0.Before(cur.EndsAt) {
		t.Fatalf("cur [%s, %s) does not cover %s", cur.StartsAt, cur.EndsAt, t0)
	}
	if prev == nil {
		t.Fatal("expected a predecessor")
	}
	if !prev.EndsAt.Equal(cur.StartsAt) {
		t.Errorf("chain gap: prev ends %s, cur starts %s", prev.EndsAt, cur.StartsAt)
	}
	if cur.StartSeq != prev.StartSeq+prev.Segments() {
		t.Errorf("StartSeq %d, want prev %d + %d", cur.StartSeq, prev.StartSeq, prev.Segments())
	}
	if cur.StartItem != prev.StartItem+int64(len(prev.ItemIDs)) {
		t.Errorf("StartItem %d, want prev %d + %d", cur.StartItem, prev.StartItem, len(prev.ItemIDs))
	}
	if cur.Version != prev.Version+1 {
		t.Errorf("versions %d after %d", cur.Version, prev.Version)
	}
	// Calling again is a no-op: same version served, nothing new stored.
	all, _ := m.ListVersions(context.Background(), "zip:77494")
	again, _, _ := s.versionAt(context.Background(), "zip:77494", t0)
	all2, _ := m.ListVersions(context.Background(), "zip:77494")
	if again.Version != cur.Version || len(all) != len(all2) {
		t.Errorf("second call changed state: v%d→v%d, %d→%d versions", cur.Version, again.Version, len(all), len(all2))
	}
}

func TestVersionAt_IdleChannelRestartsNearNow(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 3)
	s := testService(m, t0)
	first, _, err := s.versionAt(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}

	later := t0.Add(72 * time.Hour)
	s.now = func() time.Time { return later }
	cur, prev, err := s.versionAt(context.Background(), "zip:77494", later)
	if err != nil {
		t.Fatal(err)
	}
	if !later.Before(cur.EndsAt) || cur.StartsAt.After(later) {
		t.Fatalf("cur [%s, %s) does not cover %s", cur.StartsAt, cur.EndsAt, later)
	}
	// The restart happened at later − 1 h, not by chaining 72 h of dead air.
	all, _ := m.ListVersions(context.Background(), "zip:77494")
	if len(all) > 45 {
		t.Errorf("chained %d versions through idle time; expected a restart", len(all))
	}
	restart := all[first.Version] // the first version created after the idle gap
	if want := later.Add(-time.Hour); !restart.StartsAt.Equal(want) {
		t.Errorf("restart StartsAt = %s, want %s", restart.StartsAt, want)
	}
	if restart.StartSeq != first.StartSeq+first.Segments() {
		t.Errorf("counters must continue across the gap: %d vs %d", restart.StartSeq, first.StartSeq+first.Segments())
	}
	_ = prev
}

// racingStore makes the first InsertVersion lose to a rival row, as when a
// second instance created the same version concurrently.
type racingStore struct {
	*memStore
	raced bool
}

func (r *racingStore) InsertVersion(ctx context.Context, v *Version) (bool, error) {
	if !r.raced {
		r.raced = true
		rival := *v
		rival.ItemIDs = append([]int64(nil), v.ItemIDs...)
		// Rival chose a different (but valid) start: one minute earlier.
		rival.StartsAt = v.StartsAt.Add(-time.Minute)
		rival.EndsAt = v.EndsAt.Add(-time.Minute)
		_, _ = r.memStore.InsertVersion(ctx, &rival)
		return false, nil
	}
	return r.memStore.InsertVersion(ctx, v)
}

func TestVersionAt_ServesTheRowThatWonTheRace(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 200)
	rs := &racingStore{memStore: m}
	s := testService(rs, t0)
	cur, _, err := s.versionAt(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(-time.Hour - time.Minute); !cur.StartsAt.Equal(want) {
		t.Errorf("served StartsAt = %s, want rival's %s", cur.StartsAt, want)
	}
}

func TestVersionAt_NoContent(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, _, err := s.versionAt(context.Background(), "us", t0); !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/linear/ -v`
Expected: build failure — `undefined: New`, `buildItems`, `seedFor`, `Options`.

- [ ] **Step 4: Write the implementation**

`internal/linear/service.go`:

```go
package linear

import (
	"log/slog"
	"sync"
	"time"
)

// Options tune channel assembly. Zero values take the defaults below.
type Options struct {
	LineupHours     int // max content per lineup version
	MinScopeClips   int // a scope with fewer current clips falls back to its parent
	EPGHorizonHours int // how far ahead the EPG materialises versions
}

func (o Options) withDefaults() Options {
	if o.LineupHours <= 0 {
		o.LineupHours = 6
	}
	if o.MinScopeClips <= 0 {
		o.MinScopeClips = 20
	}
	if o.EPGHorizonHours <= 0 {
		o.EPGHorizonHours = 24
	}
	return o
}

// windowSegments is how many segments a live playlist lists.
const windowSegments = 6

// Service assembles channels from the store.
type Service struct {
	store Store
	opts  Options
	now   func() time.Time
	log   *slog.Logger

	mu    sync.Mutex
	cache map[string]cached // channel key → versions last served
}

type cached struct{ cur, prev *Version }

// New creates a Service.
func New(store Store, opts Options, log *slog.Logger) *Service {
	return &Service{store: store, opts: opts.withDefaults(), now: time.Now, log: log, cache: map[string]cached{}}
}

// current returns the versions serving key at t. Versions are immutable once
// stored, so a cached version that still covers t is served without a query.
func (s *Service) current(ctx context.Context, key string, t time.Time) (cur, prev *Version, err error) {
	s.mu.Lock()
	c, ok := s.cache[key]
	s.mu.Unlock()
	if ok && !t.Before(c.cur.StartsAt) && t.Before(c.cur.EndsAt) {
		return c.cur, c.prev, nil
	}
	cur, prev, err = s.versionAt(ctx, key, t)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	s.cache[key] = cached{cur: cur, prev: prev}
	s.mu.Unlock()
	return cur, prev, nil
}
```

(Add `"context"` to the import block.)

`internal/linear/lineup.go`:

```go
package linear

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

// ErrNoContent means the scope, after fallback, has no current clips.
var ErrNoContent = errors.New("no content for channel")

const (
	// historyLead is how far in the past a fresh chain starts, so the very
	// first request can already serve a full window.
	historyLead = time.Hour
	// idleGrace: a channel not requested this long past its latest version's
	// end restarts historyLead before now instead of chaining through dead
	// time nobody watched.
	idleGrace = 5 * time.Minute
	// maxChain bounds how many versions one request may create.
	maxChain = 64
)

// seedFor derives the shuffle seed of a version. Any instance computing the
// same (key, version) over the same clip set gets the same order.
func seedFor(key string, version int) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key + "|" + strconv.Itoa(version)))
	return int64(h.Sum64())
}

// buildItems picks a version's air order: a seeded shuffle of clips (sorted
// by id first so the input order is irrelevant), cut to maxMS. A lineup always
// has at least one clip, even if that clip alone exceeds maxMS.
func buildItems(clips []ClipRef, seed int64, maxMS int) (ids []int64, ms []int, segs []int) {
	order := make([]ClipRef, len(clips))
	copy(order, clips)
	sort.Slice(order, func(i, j int) bool { return order[i].ID < order[j].ID })
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	total := 0
	for _, c := range order {
		if len(ids) > 0 && total+c.TotalMS > maxMS {
			break
		}
		ids = append(ids, c.ID)
		ms = append(ms, c.TotalMS)
		segs = append(segs, c.Segments)
		total += c.TotalMS
	}
	return ids, ms, segs
}

// resolveScope walks zip → city → state → national until a scope has at
// least MinScopeClips current clips, returning the effective scope and its
// clips. The national scope is used whatever its size; if even it is empty
// the result is ErrNoContent.
func (s *Service) resolveScope(ctx context.Context, sc Scope) (Scope, []ClipRef, error) {
	for {
		clips, err := s.store.ListClips(ctx, sc)
		if err != nil {
			return Scope{}, nil, err
		}
		if len(clips) >= s.opts.MinScopeClips || sc == (Scope{}) {
			if len(clips) == 0 {
				return Scope{}, nil, ErrNoContent
			}
			return sc, clips, nil
		}
		switch {
		case sc.Zip != "":
			city, state, err := s.store.CityOfZip(ctx, sc.Zip)
			if err != nil {
				return Scope{}, nil, err
			}
			if city != "" && state != "" {
				sc = Scope{City: city, State: state}
			} else {
				sc = Scope{}
			}
		case sc.City != "":
			sc = Scope{State: sc.State}
		default:
			sc = Scope{}
		}
	}
}

// newVersion materialises version n of key starting at startsAt, continuing
// the counters of prev (nil for version 1).
func (s *Service) newVersion(ctx context.Context, key string, n int, startsAt time.Time, prev *Version) (*Version, error) {
	sc, err := ParseKey(key)
	if err != nil {
		return nil, err
	}
	eff, clips, err := s.resolveScope(ctx, sc)
	if err != nil {
		return nil, err
	}
	ids, ms, segs := buildItems(clips, seedFor(key, n), s.opts.LineupHours*3600*1000)
	v := &Version{Key: key, Version: n, Scope: eff.Key(), StartsAt: startsAt, ItemIDs: ids, ItemMS: ms, ItemSegs: segs}
	v.EndsAt = startsAt.Add(time.Duration(v.TotalMS()) * time.Millisecond)
	if prev != nil {
		v.StartSeq = prev.StartSeq + prev.Segments()
		v.StartItem = prev.StartItem + int64(len(prev.ItemIDs))
	}
	return v, nil
}

// versionAt returns the version covering t and its predecessor (nil for
// version 1), creating versions as needed. Creation races between instances
// are settled by the primary key: after every insert the chain is re-read, so
// the stored row — whoever wrote it — is the one served.
func (s *Service) versionAt(ctx context.Context, key string, t time.Time) (cur, prev *Version, err error) {
	now := s.now()
	for i := 0; i < maxChain; i++ {
		vs, err := s.store.LatestVersions(ctx, key, 2)
		if err != nil {
			return nil, nil, err
		}
		var (
			n        int
			startsAt time.Time
			last     *Version
		)
		switch {
		case len(vs) == 0:
			n, startsAt = 1, now.Add(-historyLead).Truncate(time.Minute)
		case t.Before(vs[0].StartsAt):
			return nil, nil, fmt.Errorf("time %s precedes channel %s version %d start %s", t, key, vs[0].Version, vs[0].StartsAt)
		case t.Before(vs[0].EndsAt):
			cur = &vs[0]
			if len(vs) > 1 {
				prev = &vs[1]
			}
			return cur, prev, nil
		default:
			last = &vs[0]
			n = last.Version + 1
			startsAt = last.EndsAt
			if now.After(last.EndsAt.Add(idleGrace)) {
				startsAt = now.Add(-historyLead).Truncate(time.Minute)
				if startsAt.Before(last.EndsAt) {
					startsAt = last.EndsAt
				}
			}
		}
		v, err := s.newVersion(ctx, key, n, startsAt, last)
		if err != nil {
			return nil, nil, err
		}
		if _, err := s.store.InsertVersion(ctx, v); err != nil {
			return nil, nil, err
		}
		s.log.Info("channel lineup version created", "channel", key, "version", n, "scope", v.Scope,
			"items", len(v.ItemIDs), "starts_at", v.StartsAt, "ends_at", v.EndsAt)
	}
	return nil, nil, fmt.Errorf("channel %s: could not reach %s within %d versions", key, t, maxChain)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/linear/ -v`
Expected: PASS. If `TestVersionAt_ChainsVersionsUntilCovered` fails on `maxChain`, note that 60 min / 3 min = 21 versions < 64 — the loop must re-read `LatestVersions` each iteration, not reuse `vs`.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./internal/linear/
git add internal/linear/
git commit -m "feat: deterministic lineup chain with scope fallback"
```

---

### Task 5: timeline window and playlist writer

**Files:**
- Create: `internal/linear/timeline.go`
- Create: `internal/linear/playlist.go`
- Create: `internal/linear/playlist_test.go`

**Interfaces:**
- Consumes: `Version`, `ClipSegments` (Task 4); `hls.SegmentName`, `hls.TargetDuration` (Task 1).
- Produces (unexported): `segment` struct, `windowClipIDs(cur, prev *Version, now time.Time, n int) []int64`, `window(cur, prev *Version, clips map[int64]ClipSegments, now time.Time, n int) []segment`, `writePlaylist(w io.Writer, segs []segment)`, `writeMaster(w io.Writer, mediaURI string)`.

- [ ] **Step 1: Write the failing tests**

`internal/linear/playlist_test.go`:

```go
package linear

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Two-version fixture (times relative to t0):
//
//	v1: item A (3 s, 3 s, 2 s) then item B (4 s, 4 s)  → [t0, t0+16 s)
//	v2: item C (5 s, 5 s)                                → [t0+16 s, t0+26 s)
func fixture() (v1, v2 *Version, clips map[int64]ClipSegments) {
	clips = map[int64]ClipSegments{
		1: {ID: 1, BaseURL: "https://cdn/hls/v1/A/x", SegmentMS: []int{3000, 3000, 2000}},
		2: {ID: 2, BaseURL: "https://cdn/hls/v1/B/x", SegmentMS: []int{4000, 4000}},
		3: {ID: 3, BaseURL: "https://cdn/hls/v1/C/x", SegmentMS: []int{5000, 5000}},
	}
	v1 = &Version{Key: "us", Version: 1, StartsAt: t0, EndsAt: t0.Add(16 * time.Second),
		StartSeq: 0, StartItem: 0, ItemIDs: []int64{1, 2}, ItemMS: []int{8000, 8000}, ItemSegs: []int{3, 2}}
	v2 = &Version{Key: "us", Version: 2, StartsAt: t0.Add(16 * time.Second), EndsAt: t0.Add(26 * time.Second),
		StartSeq: 5, StartItem: 2, ItemIDs: []int64{3}, ItemMS: []int{10000}, ItemSegs: []int{2}}
	return v1, v2, clips
}

func TestWindow_ListsOnlyEndedSegments(t *testing.T) {
	v1, _, clips := fixture()
	segs := window(v1, nil, clips, t0.Add(12*time.Second), 6)
	// Ended by t0+12: A0 [0,3) A1 [3,6) A2 [6,8) B0 [8,12). B1 ends at 16.
	if len(segs) != 4 {
		t.Fatalf("got %d segments, want 4", len(segs))
	}
	wantURLs := []string{
		"https://cdn/hls/v1/A/x/seg-000.ts", "https://cdn/hls/v1/A/x/seg-001.ts",
		"https://cdn/hls/v1/A/x/seg-002.ts", "https://cdn/hls/v1/B/x/seg-000.ts",
	}
	for i, s := range segs {
		if s.URL != wantURLs[i] {
			t.Errorf("seg %d url = %s, want %s", i, s.URL, wantURLs[i])
		}
		if s.Seq != int64(i) {
			t.Errorf("seg %d seq = %d", i, s.Seq)
		}
	}
	if !segs[3].FirstOfItem || segs[3].Item != 1 || segs[2].FirstOfItem {
		t.Errorf("item boundaries wrong: %+v", segs)
	}
	if !segs[3].Start.Equal(t0.Add(8 * time.Second)) {
		t.Errorf("B0 start = %s, want t0+8s", segs[3].Start)
	}
}

func TestWritePlaylist_Golden(t *testing.T) {
	v1, _, clips := fixture()
	segs := window(v1, nil, clips, t0.Add(12*time.Second), 6)
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	want := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-DISCONTINUITY-SEQUENCE:0
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:00.000Z
#EXTINF:3.000,
https://cdn/hls/v1/A/x/seg-000.ts
#EXTINF:3.000,
https://cdn/hls/v1/A/x/seg-001.ts
#EXTINF:2.000,
https://cdn/hls/v1/A/x/seg-002.ts
#EXT-X-DISCONTINUITY
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:08.000Z
#EXTINF:4.000,
https://cdn/hls/v1/B/x/seg-000.ts
`
	if buf.String() != want {
		t.Errorf("playlist:\n%s\nwant:\n%s", buf.String(), want)
	}
	if strings.Contains(buf.String(), "ENDLIST") {
		t.Error("live playlist must not contain EXT-X-ENDLIST")
	}
}

func TestWindow_SpansVersionBoundaryAndCountsRemovedDiscontinuities(t *testing.T) {
	v1, v2, clips := fixture()

	// t0+21: C0 [16,21) has ended. Window of 3 → B0 B1 C0.
	segs := window(v2, v1, clips, t0.Add(21*time.Second), 3)
	if len(segs) != 3 || segs[0].URL != "https://cdn/hls/v1/B/x/seg-000.ts" || segs[2].URL != "https://cdn/hls/v1/C/x/seg-000.ts" {
		t.Fatalf("window = %+v", segs)
	}
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	out := buf.String()
	// First segment is B0: seq 3, item 1 and its first segment → the only
	// discontinuity tag ever removed is none (B's own tag is still listed).
	if !strings.Contains(out, "#EXT-X-MEDIA-SEQUENCE:3\n") || !strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE:0\n") {
		t.Errorf("counters wrong:\n%s", out)
	}
	if strings.Count(out, "#EXT-X-DISCONTINUITY\n") != 2 { // before B0 and before C0
		t.Errorf("want 2 discontinuity tags:\n%s", out)
	}

	// t0+26: C1 has ended. Window of 3 → B1 C0 C1: B's tag was removed.
	segs = window(v2, v1, clips, t0.Add(26*time.Second), 3)
	buf.Reset()
	writePlaylist(&buf, segs)
	out = buf.String()
	if !strings.Contains(out, "#EXT-X-MEDIA-SEQUENCE:4\n") || !strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n") {
		t.Errorf("counters wrong:\n%s", out)
	}
}

func TestWindowClipIDs_CoversWindowAndPrevTail(t *testing.T) {
	v1, v2, clips := fixture()
	ids := windowClipIDs(v2, v1, t0.Add(17*time.Second), 6)
	for _, want := range []int64{1, 2, 3} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("ids %v missing %d", ids, want)
		}
	}
	_ = clips
}

var seqRe = regexp.MustCompile(`#EXT-X-MEDIA-SEQUENCE:(\d+)\n#EXT-X-DISCONTINUITY-SEQUENCE:(\d+)\n`)

func TestPlaylist_CountersNeverDecrease(t *testing.T) {
	v1, v2, clips := fixture()
	lastSeq, lastDisc := int64(-1), int64(-1)
	for now := t0.Add(3 * time.Second); now.Before(v2.EndsAt); now = now.Add(time.Second) {
		cur, prev := v1, (*Version)(nil)
		if !now.Before(v2.StartsAt) {
			cur, prev = v2, v1
		}
		segs := window(cur, prev, clips, now, 3)
		var buf bytes.Buffer
		writePlaylist(&buf, segs)
		m := seqRe.FindStringSubmatch(buf.String())
		if m == nil {
			t.Fatalf("no counters at %s:\n%s", now, buf.String())
		}
		seq, _ := strconv.ParseInt(m[1], 10, 64)
		disc, _ := strconv.ParseInt(m[2], 10, 64)
		if seq < lastSeq || disc < lastDisc {
			t.Errorf("at %s counters went backwards: seq %d→%d disc %d→%d", now, lastSeq, seq, lastDisc, disc)
		}
		lastSeq, lastDisc = seq, disc
	}
}

func TestWriteMaster(t *testing.T) {
	var buf bytes.Buffer
	writeMaster(&buf, "live.m3u8?zip=77494")
	want := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1400000,AVERAGE-BANDWIDTH=1100000,CODECS=\"avc1.640028,mp4a.40.2\",RESOLUTION=1920x1080,FRAME-RATE=30.000\n" +
		"live.m3u8?zip=77494\n"
	if buf.String() != want {
		t.Errorf("master:\n%s\nwant:\n%s", buf.String(), want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/linear/ -run 'TestWindow|TestWrite|TestPlaylist' -v`
Expected: build failure — `undefined: window`, `writePlaylist`, `windowClipIDs`, `writeMaster`.

- [ ] **Step 3: Write the implementation**

`internal/linear/timeline.go`:

```go
package linear

import (
	"time"

	"github.com/dwellingtw/backend/internal/hls"
)

// segment is one entry of a live playlist.
type segment struct {
	URL         string
	DurMS       int
	Start       time.Time
	Seq         int64 // media sequence number
	Item        int64 // global item index across the chain
	FirstOfItem bool
}

// itemAt returns the index of the item airing at t within v, or -1 when t is
// outside the version.
func itemAt(v *Version, t time.Time) int {
	off := t.Sub(v.StartsAt).Milliseconds()
	if off < 0 {
		return -1
	}
	var cum int64
	for i, ms := range v.ItemMS {
		cum += int64(ms)
		if off < cum {
			return i
		}
	}
	return -1
}

// itemRange returns the item indexes [from, to] of v whose segments a window
// of n ending at now could need: the item airing at now (or the last item if
// now is past the version) and the n before it, since an item has at least
// one segment.
func itemRange(v *Version, now time.Time, n int) (from, to int) {
	to = itemAt(v, now)
	if to < 0 {
		to = len(v.ItemIDs) - 1
	}
	return max(to-n, 0), to
}

// windowClipIDs lists the clip ids window may need, so the caller can fetch
// them in one query.
func windowClipIDs(cur, prev *Version, now time.Time, n int) []int64 {
	from, to := itemRange(cur, now, n)
	ids := append([]int64(nil), cur.ItemIDs[from:to+1]...)
	if prev != nil {
		pfrom := max(len(prev.ItemIDs)-n, 0)
		ids = append(ids, prev.ItemIDs[pfrom:]...)
	}
	return ids
}

// expandItems yields the segments of items [from, to] of v.
func expandItems(v *Version, clips map[int64]ClipSegments, from, to int) []segment {
	var out []segment
	var startMS int64
	seq := v.StartSeq
	for i := 0; i < from; i++ {
		startMS += int64(v.ItemMS[i])
		seq += int64(v.ItemSegs[i])
	}
	for i := from; i <= to && i < len(v.ItemIDs); i++ {
		c := clips[v.ItemIDs[i]]
		t := v.StartsAt.Add(time.Duration(startMS) * time.Millisecond)
		for j, ms := range c.SegmentMS {
			out = append(out, segment{
				URL:         c.BaseURL + "/" + hls.SegmentName(j),
				DurMS:       ms,
				Start:       t,
				Seq:         seq,
				Item:        v.StartItem + int64(i),
				FirstOfItem: j == 0,
			})
			t = t.Add(time.Duration(ms) * time.Millisecond)
			seq++
		}
		startMS += int64(v.ItemMS[i])
	}
	return out
}

// ended keeps the segments that have finished by now.
func ended(segs []segment, now time.Time) []segment {
	out := segs[:0:0]
	for _, s := range segs {
		if !s.Start.Add(time.Duration(s.DurMS) * time.Millisecond).After(now) {
			out = append(out, s)
		}
	}
	return out
}

// window returns the last n segments that have ended by now, from cur and —
// when cur has too few yet — the tail of prev.
func window(cur, prev *Version, clips map[int64]ClipSegments, now time.Time, n int) []segment {
	from, to := itemRange(cur, now, n)
	segs := ended(expandItems(cur, clips, from, to), now)
	if len(segs) < n && prev != nil {
		pfrom := max(len(prev.ItemIDs)-n, 0)
		psegs := ended(expandItems(prev, clips, pfrom, len(prev.ItemIDs)-1), now)
		segs = append(psegs, segs...)
	}
	if len(segs) > n {
		segs = segs[len(segs)-n:]
	}
	return segs
}
```

`internal/linear/playlist.go`:

```go
package linear

import (
	"fmt"
	"io"

	"github.com/dwellingtw/backend/internal/hls"
)

// writePlaylist renders the live media playlist for a non-empty window.
//
// Every item boundary is an EXT-X-DISCONTINUITY (timestamps restart per
// clip). EXT-X-DISCONTINUITY-SEQUENCE counts the tags that belong to segments
// already gone from the window: with g the global item index of the first
// listed segment, that is g−1 tags for items 1..g−1, plus item g's own tag
// when the window starts mid-item.
func writePlaylist(w io.Writer, segs []segment) {
	first := segs[0]
	var removed int64
	if first.Item > 0 {
		removed = first.Item - 1
		if !first.FirstOfItem {
			removed++
		}
	}
	fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-INDEPENDENT-SEGMENTS\n", hls.TargetDuration)
	fmt.Fprintf(w, "#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", first.Seq, removed)
	for i, s := range segs {
		disc := s.FirstOfItem && s.Item > 0
		if disc {
			io.WriteString(w, "#EXT-X-DISCONTINUITY\n")
		}
		if i == 0 || disc {
			fmt.Fprintf(w, "#EXT-X-PROGRAM-DATE-TIME:%s\n", s.Start.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		fmt.Fprintf(w, "#EXTINF:%.3f,\n%s\n", float64(s.DurMS)/1000, s.URL)
	}
}

// writeMaster renders the master playlist. All clips share one rendition, so
// there is a single variant; the attributes describe the listing encode
// (H.264 High 4.0 1080p30, AAC-LC).
func writeMaster(w io.Writer, mediaURI string) {
	io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	io.WriteString(w, "#EXT-X-STREAM-INF:BANDWIDTH=1400000,AVERAGE-BANDWIDTH=1100000,CODECS=\"avc1.640028,mp4a.40.2\",RESOLUTION=1920x1080,FRAME-RATE=30.000\n")
	fmt.Fprintf(w, "%s\n", mediaURI)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/linear/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/linear/
git add internal/linear/timeline.go internal/linear/playlist.go internal/linear/playlist_test.go
git commit -m "feat: sliding live playlist over the lineup timeline"
```

---

### Task 6: `Service.Playlist` and the HTTP handler

**Files:**
- Modify: `internal/linear/service.go` (add `Playlist`)
- Create: `internal/linear/handler.go`
- Create: `internal/linear/service_test.go`
- Create: `internal/linear/handler_test.go`

**Interfaces:**
- Consumes: `current`, `windowClipIDs`, `window`, `writePlaylist`, `writeMaster`, `ParseScope`, `ErrBadScope`, `ErrNoContent`.
- Produces: `(*Service).Playlist(ctx, sc Scope, now time.Time) ([]byte, error)`, `linear.NewHandler(svc *Service, log *slog.Logger) *Handler`, `(*Handler).Register(mux *http.ServeMux)` mounting `GET /channels/master.m3u8`, `GET /channels/live.m3u8`, `GET /channels/epg.json` (epg handler wired in Task 7 — register the route here with a handler that returns 501 until then is NOT allowed; instead register only the two playlist routes here and add the third in Task 7).

- [ ] **Step 1: Write the failing tests**

`internal/linear/service_test.go`:

```go
package linear

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPlaylist_IsDeterministicAcrossInstances(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	now := t0.Add(90 * time.Second)
	a := testService(m, now)
	b := testService(m, now)

	pa, err := a.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pa, pb) {
		t.Errorf("instances disagree:\n%s\n---\n%s", pa, pb)
	}
	if !strings.HasPrefix(string(pa), "#EXTM3U\n") || strings.Count(string(pa), "#EXTINF:") != windowSegments {
		t.Errorf("unexpected playlist:\n%s", pa)
	}
}

func TestPlaylist_AdvancesWithTime(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	p1, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, t0.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(p1, p2) {
		t.Error("playlist did not advance after 30 s")
	}
}

func TestPlaylist_NoContent(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, err := s.Playlist(context.Background(), Scope{}, t0); !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}
```

`internal/linear/handler_test.go`:

```go
package linear

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, store Store, now time.Time, path string) *httptest.ResponseRecorder {
	t.Helper()
	svc := testService(store, now)
	mux := http.NewServeMux()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHandler_LivePlaylist(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	rec := serve(t, m, t0, "/channels/live.m3u8?zip=77494")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=2" {
		t.Errorf("cache control = %q", cc)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
	if !strings.HasPrefix(rec.Body.String(), "#EXTM3U\n") {
		t.Errorf("body:\n%s", rec.Body.String())
	}
}

func TestHandler_Master(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/channels/master.m3u8?zip=77494")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "\nlive.m3u8?zip=77494\n") {
		t.Errorf("master must reference the media playlist with the same query:\n%s", rec.Body.String())
	}
	rec = serve(t, newMemStore(), t0, "/channels/master.m3u8")
	if !strings.Contains(rec.Body.String(), "\nlive.m3u8\n") {
		t.Errorf("national master:\n%s", rec.Body.String())
	}
}

func TestHandler_BadFilterIs400(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/channels/live.m3u8?city=Katy")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_NoContentIs503(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/channels/live.m3u8")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", rec.Code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/linear/ -run 'TestPlaylist_|TestHandler' -v`
Expected: build failure — `Playlist` / `NewHandler` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/linear/service.go` (add `"bytes"`, `"fmt"` imports):

```go
// Playlist renders the live media playlist of sc at now.
func (s *Service) Playlist(ctx context.Context, sc Scope, now time.Time) ([]byte, error) {
	key := sc.Key()
	cur, prev, err := s.current(ctx, key, now)
	if err != nil {
		return nil, err
	}
	ids := windowClipIDs(cur, prev, now, windowSegments)
	clips, err := s.store.ClipsByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := clips[id]; !ok {
			return nil, fmt.Errorf("channel %s v%d references missing clip %d", key, cur.Version, id)
		}
	}
	segs := window(cur, prev, clips, now, windowSegments)
	if len(segs) == 0 {
		return nil, ErrNoContent
	}
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	return buf.Bytes(), nil
}
```

`internal/linear/handler.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/linear/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/linear/
git add internal/linear/
git commit -m "feat: live playlist service and channel HTTP endpoints"
```

---

### Task 7: EPG

**Files:**
- Create: `internal/linear/epg.go`
- Create: `internal/linear/epg_test.go`
- Modify: `internal/linear/handler.go` (register `GET /channels/epg.json`)
- Modify: `internal/linear/handler_test.go` (add EPG route test)

**Interfaces:**
- Consumes: `versionAt`, `Store.ListVersions`, `Store.ListingsByClipID`, `ParseKey`, `Scope.Name`.
- Produces: `linear.EPG{Channel Channel; Programs []Program}`, `linear.Channel{Key, Scope, Name string}`, `linear.Program{Start, End time.Time; Title, Description string; Listings int}`, `(*Service).EPG(ctx, sc Scope, now time.Time) (EPG, error)`.

- [ ] **Step 1: Write the failing tests**

`internal/linear/epg_test.go`:

```go
package linear

import (
	"context"
	"testing"
	"time"
)

func TestEPG_ThirtyMinuteBlocksWithListingSummary(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40) // 40 one-minute clips, prices 300,000 … 339,000
	s := testService(m, t0.Add(7*time.Minute))

	e, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if e.Channel.Key != "zip:77494" || e.Channel.Scope != "zip:77494" || e.Channel.Name != "Homes for sale in 77494" {
		t.Errorf("channel = %+v", e.Channel)
	}
	if len(e.Programs) == 0 {
		t.Fatal("no programs")
	}
	first := e.Programs[0]
	if first.Start.Minute()%30 != 0 || first.Start.Second() != 0 {
		t.Errorf("first program start %s is not on a half hour", first.Start)
	}
	if !first.End.Equal(first.Start.Add(30 * time.Minute)) {
		t.Errorf("program length = %s", first.End.Sub(first.Start))
	}
	if first.Title != "Homes for sale in 77494" {
		t.Errorf("title = %q", first.Title)
	}
	if first.Listings < 1 || first.Listings > 30 {
		t.Errorf("listings = %d", first.Listings)
	}
	if first.Description == "" || first.Description[len(first.Description)-1] == ' ' {
		t.Errorf("description = %q", first.Description)
	}
	// Coverage extends at least to now + horizon (24 h) minus one block.
	last := e.Programs[len(e.Programs)-1]
	if last.End.Before(t0.Add(7*time.Minute + 23*time.Hour)) {
		t.Errorf("EPG ends at %s, want ≥ now + 23 h", last.End)
	}
	for i := 1; i < len(e.Programs); i++ {
		if e.Programs[i].Start.Before(e.Programs[i-1].End) {
			t.Errorf("programs overlap at %d", i)
		}
	}
}

func TestEPG_DescriptionFormat(t *testing.T) {
	if got := describe(3, 285000, 1150000); got != "3 listings · $285,000–$1,150,000" {
		t.Errorf("describe = %q", got)
	}
	if got := describe(1, 500000, 500000); got != "1 listing · $500,000" {
		t.Errorf("describe = %q", got)
	}
	if got := describe(2, 0, 0); got != "2 listings" {
		t.Errorf("describe = %q", got)
	}
}
```

Add to `internal/linear/handler_test.go`:

```go
func TestHandler_EPG(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	rec := serve(t, m, t0, "/channels/epg.json?zip=77494")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("cache control = %q", cc)
	}
	if !strings.Contains(rec.Body.String(), `"programs":[{"start":"`) {
		t.Errorf("body:\n%s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/linear/ -run 'TestEPG|TestHandler_EPG' -v`
Expected: build failure — `EPG`, `describe` undefined.

- [ ] **Step 3: Write the implementation**

`internal/linear/epg.go`:

```go
package linear

import (
	"context"
	"strconv"
	"time"
)

// programBlock is the EPG granularity.
const programBlock = 30 * time.Minute

// EPG is the schedule of one channel.
type EPG struct {
	Channel  Channel   `json:"channel"`
	Programs []Program `json:"programs"`
}

// Channel identifies a channel and the scope it actually airs.
type Channel struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
	Name  string `json:"name"`
}

// Program is one EPG block.
type Program struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Listings    int       `json:"listings"`
}

// EPG returns programBlock-sized programs from the channel's first version
// through now + EPGHorizonHours, materialising versions as far as needed.
func (s *Service) EPG(ctx context.Context, sc Scope, now time.Time) (EPG, error) {
	key := sc.Key()
	horizon := now.Add(time.Duration(s.opts.EPGHorizonHours) * time.Hour)
	latest, _, err := s.versionAt(ctx, key, horizon)
	if err != nil {
		return EPG{}, err
	}
	versions, err := s.store.ListVersions(ctx, key)
	if err != nil {
		return EPG{}, err
	}

	// Every item start within the range, with the scope it airs under.
	type aired struct {
		id    int64
		start time.Time
		scope string
	}
	var items []aired
	var ids []int64
	for _, v := range versions {
		t := v.StartsAt
		for i, id := range v.ItemIDs {
			if !t.After(horizon) {
				items = append(items, aired{id: id, start: t, scope: v.Scope})
				ids = append(ids, id)
			}
			t = t.Add(time.Duration(v.ItemMS[i]) * time.Millisecond)
		}
	}
	listings, err := s.store.ListingsByClipID(ctx, ids)
	if err != nil {
		return EPG{}, err
	}
	price := make(map[int64]int64, len(listings))
	for _, l := range listings {
		price[l.ClipID] = l.Price
	}

	effScope, _ := ParseKey(latest.Scope)
	e := EPG{Channel: Channel{Key: key, Scope: latest.Scope, Name: effScope.Name()}}
	i := 0
	for b := versions[0].StartsAt.Truncate(programBlock); b.Before(horizon); b = b.Add(programBlock) {
		end := b.Add(programBlock)
		var n int
		var lo, hi int64
		scope := ""
		for ; i < len(items) && items[i].start.Before(end); i++ {
			if items[i].start.Before(b) {
				continue
			}
			n++
			scope = items[i].scope
			if p := price[items[i].id]; p > 0 {
				if lo == 0 || p < lo {
					lo = p
				}
				if p > hi {
					hi = p
				}
			}
		}
		if n == 0 {
			continue // idle gap between versions: nothing aired
		}
		blockScope, _ := ParseKey(scope)
		e.Programs = append(e.Programs, Program{
			Start: b, End: end, Title: blockScope.Name(), Description: describe(n, lo, hi), Listings: n,
		})
	}
	return e, nil
}

// describe formats "12 listings · $285,000–$1,150,000". Zero prices are omitted.
func describe(n int, lo, hi int64) string {
	s := strconv.Itoa(n) + " listing"
	if n != 1 {
		s += "s"
	}
	switch {
	case lo > 0 && hi > lo:
		s += " · $" + humanInt(lo) + "–$" + humanInt(hi)
	case lo > 0:
		s += " · $" + humanInt(lo)
	}
	return s
}

func humanInt(n int64) string {
	d := strconv.FormatInt(n, 10)
	var out []byte
	for i := 0; i < len(d); i++ {
		if i > 0 && (len(d)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d[i])
	}
	return string(out)
}
```

In `internal/linear/handler.go`, add to `Register`:

```go
	mux.HandleFunc("GET /channels/epg.json", h.epg)
```

and the handler:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/linear/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/linear/
git add internal/linear/
git commit -m "feat: channel EPG in 30-minute blocks"
```

---

### Task 8: PostgreSQL store

**Files:**
- Create: `internal/linear/repository.go`
- Create: `internal/linear/repository_test.go`

**Interfaces:**
- Consumes: `Store`, `Version`, `ClipRef`, `ClipSegments`, `Listing`, `Scope`; `hls.Clip`.
- Produces: `linear.NewRepository(pool *pgxpool.Pool) *Repository` implementing `Store`, plus `(*Repository).SetVideoHLS(ctx, zpid, contentHash, baseURL string, clip hls.Clip) error` and the pure `scopeWhere(s Scope) (string, []any)`.

- [ ] **Step 1: Write the failing test for the pure SQL builder**

`internal/linear/repository_test.go`:

```go
package linear

import (
	"reflect"
	"testing"
)

func TestScopeWhere(t *testing.T) {
	tests := []struct {
		scope Scope
		where string
		args  []any
	}{
		{Scope{}, "", nil},
		{Scope{Zip: "77494"}, " AND p.zip = $1", []any{"77494"}},
		{Scope{City: "katy", State: "tx"}, " AND lower(p.city) = $1 AND lower(p.state) = $2", []any{"katy", "tx"}},
		{Scope{State: "tx"}, " AND lower(p.state) = $1", []any{"tx"}},
	}
	for _, tt := range tests {
		where, args := scopeWhere(tt.scope)
		if where != tt.where || !reflect.DeepEqual(args, tt.args) {
			t.Errorf("%+v: got %q %v, want %q %v", tt.scope, where, args, tt.where, tt.args)
		}
	}
}

// Compile-time check that the repository satisfies the service's store.
var _ Store = (*Repository)(nil)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/linear/ -run TestScopeWhere -v`
Expected: build failure — `scopeWhere`, `Repository` undefined.

- [ ] **Step 3: Write the implementation**

`internal/linear/repository.go`:

```go
package linear

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/hls"
)

// Repository is the PostgreSQL Store.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a Repository.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// scopeWhere renders the scope as extra AND-conditions on the properties
// alias p, with 1-based positional args. Empty for the national scope.
func scopeWhere(s Scope) (string, []any) {
	switch {
	case s.Zip != "":
		return " AND p.zip = $1", []any{s.Zip}
	case s.City != "":
		return " AND lower(p.city) = $1 AND lower(p.state) = $2", []any{s.City, s.State}
	case s.State != "":
		return " AND lower(p.state) = $1", []any{s.State}
	}
	return "", nil
}

// SetVideoHLS records (or refreshes) the segment layout of a render.
func (r *Repository) SetVideoHLS(ctx context.Context, zpid, contentHash, baseURL string, clip hls.Clip) error {
	const q = `
INSERT INTO video_hls (zpid, content_hash, base_url, segment_ms, total_ms)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (zpid, content_hash) DO UPDATE SET
    base_url = EXCLUDED.base_url, segment_ms = EXCLUDED.segment_ms, total_ms = EXCLUDED.total_ms`
	if _, err := r.pool.Exec(ctx, q, zpid, contentHash, baseURL, int32s(clip.SegmentMS), clip.TotalMS); err != nil {
		return fmt.Errorf("set video hls zpid=%s: %w", zpid, err)
	}
	return nil
}

// ListClips implements Store: current clips (the render the property row
// points at) in scope, ordered by id.
func (r *Repository) ListClips(ctx context.Context, s Scope) ([]ClipRef, error) {
	where, args := scopeWhere(s)
	q := `
SELECT h.id, h.total_ms, cardinality(h.segment_ms)
  FROM video_hls h
  JOIN properties p ON p.zpid = h.zpid
 WHERE p.video_status = 'ready' AND p.video_content_hash = h.content_hash` + where + `
 ORDER BY h.id`
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list clips: %w", err)
	}
	defer rows.Close()
	var out []ClipRef
	for rows.Next() {
		var c ClipRef
		if err := rows.Scan(&c.ID, &c.TotalMS, &c.Segments); err != nil {
			return nil, fmt.Errorf("scan clip: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CityOfZip implements Store.
func (r *Repository) CityOfZip(ctx context.Context, zip string) (string, string, error) {
	const q = `
SELECT lower(city), lower(state)
  FROM properties
 WHERE zip = $1 AND city IS NOT NULL AND city <> '' AND state IS NOT NULL AND state <> ''
 GROUP BY 1, 2
 ORDER BY count(*) DESC
 LIMIT 1`
	var city, state string
	err := r.pool.QueryRow(ctx, q, zip).Scan(&city, &state)
	if err == pgx.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("city of zip %s: %w", zip, err)
	}
	return city, state, nil
}

const versionColumns = `channel_key, version, scope, starts_at, ends_at, start_seq, start_item, item_ids, item_ms, item_segs`

func scanVersions(rows pgx.Rows) ([]Version, error) {
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var ms, segs []int32
		if err := rows.Scan(&v.Key, &v.Version, &v.Scope, &v.StartsAt, &v.EndsAt, &v.StartSeq, &v.StartItem, &v.ItemIDs, &ms, &segs); err != nil {
			return nil, fmt.Errorf("scan lineup: %w", err)
		}
		v.ItemMS, v.ItemSegs = ints(ms), ints(segs)
		out = append(out, v)
	}
	return out, rows.Err()
}

// LatestVersions implements Store.
func (r *Repository) LatestVersions(ctx context.Context, key string, n int) ([]Version, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+versionColumns+` FROM channel_lineups WHERE channel_key = $1 ORDER BY version DESC LIMIT $2`, key, n)
	if err != nil {
		return nil, fmt.Errorf("latest lineups %s: %w", key, err)
	}
	return scanVersions(rows)
}

// ListVersions implements Store.
func (r *Repository) ListVersions(ctx context.Context, key string) ([]Version, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+versionColumns+` FROM channel_lineups WHERE channel_key = $1 ORDER BY version ASC`, key)
	if err != nil {
		return nil, fmt.Errorf("list lineups %s: %w", key, err)
	}
	return scanVersions(rows)
}

// InsertVersion implements Store. The primary key settles creation races.
func (r *Repository) InsertVersion(ctx context.Context, v *Version) (bool, error) {
	const q = `
INSERT INTO channel_lineups (` + versionColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (channel_key, version) DO NOTHING`
	tag, err := r.pool.Exec(ctx, q, v.Key, v.Version, v.Scope, v.StartsAt, v.EndsAt, v.StartSeq, v.StartItem,
		v.ItemIDs, int32s(v.ItemMS), int32s(v.ItemSegs))
	if err != nil {
		return false, fmt.Errorf("insert lineup %s v%d: %w", v.Key, v.Version, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClipsByID implements Store.
func (r *Repository) ClipsByID(ctx context.Context, ids []int64) (map[int64]ClipSegments, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, base_url, segment_ms FROM video_hls WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("clips by id: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]ClipSegments, len(ids))
	for rows.Next() {
		var c ClipSegments
		var ms []int32
		if err := rows.Scan(&c.ID, &c.BaseURL, &ms); err != nil {
			return nil, fmt.Errorf("scan clip segments: %w", err)
		}
		c.SegmentMS = ints(ms)
		out[c.ID] = c
	}
	return out, rows.Err()
}

// ListingsByClipID implements Store.
func (r *Repository) ListingsByClipID(ctx context.Context, ids []int64) ([]Listing, error) {
	const q = `
SELECT h.id, p.zpid, COALESCE(p.sale_price, 0), COALESCE(p.city, ''), COALESCE(p.state, '')
  FROM video_hls h JOIN properties p ON p.zpid = h.zpid
 WHERE h.id = ANY($1)`
	rows, err := r.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("listings by clip: %w", err)
	}
	defer rows.Close()
	var out []Listing
	for rows.Next() {
		var l Listing
		if err := rows.Scan(&l.ClipID, &l.ZPID, &l.Price, &l.City, &l.State); err != nil {
			return nil, fmt.Errorf("scan listing: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func int32s(in []int) []int32 {
	out := make([]int32, len(in))
	for i, v := range in {
		out[i] = int32(v)
	}
	return out
}

func ints(in []int32) []int {
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}
```

- [ ] **Step 4: Run the tests and build**

Run: `go test ./internal/linear/ -v && go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/linear/
git add internal/linear/repository.go internal/linear/repository_test.go
git commit -m "feat: postgres store for clips and channel lineups"
```

---

### Task 9: scheduler segments every new render

**Files:**
- Modify: `internal/scheduler/scheduler.go` (`Scheduler` struct, `renderVideo`; add `EnableHLS`, `segmentVideo`)
- Modify: `internal/scheduler/scheduler_test.go` (add fakes + test)

**Interfaces:**
- Consumes: `hls.Clip`, `hls.Prefix`, `hls.Upload`, `hls.SegmentName`, `hls.IndexName`; `Scheduler.bunny` (uploader), `s.cfg.Concurrency.Images`.
- Produces: `(*Scheduler).EnableHLS(seg Segmenter, rec HLSRecorder)` with `type Segmenter interface{ Segment(ctx, mp4Path, outDir string) (hls.Clip, error) }` and `type HLSRecorder interface{ SetVideoHLS(ctx, zpid, contentHash, baseURL string, clip hls.Clip) error }` (satisfied by `*hls.Segmenter` and `*linear.Repository`).

- [ ] **Step 1: Write the failing test**

Append to `internal/scheduler/scheduler_test.go` (add imports `"path/filepath"` and `"github.com/dwellingtw/backend/internal/hls"` if not present):

```go
type fakeSegmenter struct{ calls atomic.Int64 }

func (f *fakeSegmenter) Segment(_ context.Context, _ string, outDir string) (hls.Clip, error) {
	f.calls.Add(1)
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(outDir, hls.SegmentName(i)), []byte("ts"), 0o644); err != nil {
			return hls.Clip{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, hls.IndexName), []byte("#EXTM3U\n"), 0o644); err != nil {
		return hls.Clip{}, err
	}
	return hls.Clip{SegmentMS: []int{3000, 2000}, TotalMS: 5000}, nil
}

type fakeHLSRecorder struct {
	mu       sync.Mutex
	recorded map[string]string // zpid → base URL
}

func (r *fakeHLSRecorder) SetVideoHLS(_ context.Context, zpid, _ string, baseURL string, _ hls.Clip) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recorded == nil {
		r.recorded = map[string]string{}
	}
	r.recorded[zpid] = baseURL
	return nil
}

func TestRunCycle_SegmentsRenderedVideos(t *testing.T) {
	imgSrv := jpegServer(t)
	props := []property.Property{{
		ZPID: "ZP1", Address: "addr",
		ImageURLs: []string{imgSrv.URL + "/a.jpg"},
		DetailURL: "https://www.zillow.com/x/",
	}}
	up := &fakeUploader{}
	rec := &fakeHLSRecorder{}
	seg := &fakeSegmenter{}
	s := New(baseConfig(), &fakeSearch{props: props}, up, &fakeStore{}, &fakeZips{queue: []string{"33950"}}, &fakeRenderer{}, testLogger())
	s.EnableHLS(seg, rec)

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seg.calls.Load() != 1 {
		t.Errorf("segmenter calls = %d, want 1", seg.calls.Load())
	}
	base := rec.recorded["ZP1"]
	if !strings.HasPrefix(base, "https://cdn.example/hls/v1/ZP1/") {
		t.Errorf("recorded base url = %q", base)
	}
	var segs, idx int
	for _, p := range up.paths() {
		switch {
		case strings.HasPrefix(p, "hls/v1/ZP1/") && strings.HasSuffix(p, ".ts"):
			segs++
		case strings.HasPrefix(p, "hls/v1/ZP1/") && strings.HasSuffix(p, "/index.m3u8"):
			idx++
		}
	}
	if segs != 2 || idx != 1 {
		t.Errorf("uploaded %d segments and %d index files, want 2 and 1: %v", segs, idx, up.paths())
	}
}

func TestRunCycle_WithoutHLSDoesNotSegment(t *testing.T) {
	imgSrv := jpegServer(t)
	props := []property.Property{{ZPID: "ZP1", Address: "addr", ImageURLs: []string{imgSrv.URL + "/a.jpg"}}}
	up := &fakeUploader{}
	s := New(baseConfig(), &fakeSearch{props: props}, up, &fakeStore{}, &fakeZips{queue: []string{"33950"}}, &fakeRenderer{}, testLogger())
	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range up.paths() {
		if strings.HasPrefix(p, "hls/") {
			t.Errorf("unexpected hls upload %s", p)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/scheduler/ -run 'TestRunCycle_SegmentsRenderedVideos|TestRunCycle_WithoutHLS' -v`
Expected: build failure — `EnableHLS` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/scheduler/scheduler.go`:

Add the import `"github.com/dwellingtw/backend/internal/hls"`.

After the `Renderer` interface add:

```go
// Segmenter cuts a rendered MP4 into HLS segments (hls.Segmenter).
type Segmenter interface {
	Segment(ctx context.Context, mp4Path, outDir string) (hls.Clip, error)
}

// HLSRecorder stores a clip's segment layout for the linear channels
// (linear.Repository).
type HLSRecorder interface {
	SetVideoHLS(ctx context.Context, zpid, contentHash, baseURL string, clip hls.Clip) error
}
```

Add two fields to `Scheduler`:

```go
	segmenter Segmenter   // nil: channel segmentation disabled
	hls       HLSRecorder
```

Add the method after `Stop`:

```go
// EnableHLS turns on channel segmentation of newly rendered videos. Call
// before Start.
func (s *Scheduler) EnableHLS(seg Segmenter, rec HLSRecorder) {
	s.segmenter, s.hls = seg, rec
}
```

In `renderVideo`, replace the final log line

```go
	s.log.Info("video ready", "zpid", p.ZPID, "url", cdnURL, "duration_secs", dur)
```

with

```go
	s.log.Info("video ready", "zpid", p.ZPID, "url", cdnURL, "duration_secs", dur)

	if s.segmenter != nil {
		s.segmentVideo(ctx, p.ZPID, hash, outPath, workDir)
	}
}

// segmentVideo cuts the rendered MP4 into HLS segments, uploads them under an
// immutable prefix and records the layout. Failures are logged only: the MP4
// is already ready for the VOD feed, and cmd/backfill-hls retries the clip.
func (s *Scheduler) segmentVideo(ctx context.Context, zpid, hash, mp4Path, workDir string) {
	dir := filepath.Join(workDir, "hls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.log.Error("hls work dir", "zpid", zpid, "error", err)
		return
	}
	clip, err := s.segmenter.Segment(ctx, mp4Path, dir)
	if err != nil {
		s.log.Error("video segment failed", "zpid", zpid, "error", err)
		return
	}
	base, err := hls.Upload(ctx, s.bunny, dir, hls.Prefix(zpid, hash), clip, s.cfg.Concurrency.Images)
	if err != nil {
		s.log.Error("segment upload failed", "zpid", zpid, "error", err)
		return
	}
	if err := s.hls.SetVideoHLS(ctx, zpid, hash, base, clip); err != nil {
		s.log.Error("record segments failed", "zpid", zpid, "error", err)
		return
	}
	s.log.Info("video segmented", "zpid", zpid, "segments", len(clip.SegmentMS), "base_url", base)
```

(The closing brace of the original `renderVideo` becomes the closing brace of `segmentVideo`; check the braces balance — `renderVideo` ends right after the `if s.segmenter != nil { … }` block.)

- [ ] **Step 4: Run the whole scheduler suite**

Run: `go test ./internal/scheduler/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/scheduler/
git add internal/scheduler/
git commit -m "feat: segment newly rendered videos for the linear channels"
```

---

### Task 10: `cmd/backfill-hls`

**Files:**
- Create: `cmd/backfill-hls/main.go`

**Interfaces:**
- Consumes: `db.Connect`, `bunny.New`, `hls.NewSegmenter`, `hls.Upload`, `hls.Prefix`, `linear.NewRepository(...).SetVideoHLS`.

- [ ] **Step 1: Write the command**

`cmd/backfill-hls/main.go`:

```go
// Command backfill-hls segments every ready listing video that the linear
// channels cannot air yet: properties whose current render has no video_hls
// row. It downloads the MP4 from the CDN, remuxes it into TS segments,
// uploads them under hls/v1/<zpid>/<hash8>/ and records the layout.
//
// The scheduler does the same for every new render; this tool clears the
// backlog that existed before channels shipped and retries clips whose
// segmentation failed. It costs no Zillow API quota.
//
// Reads DATABASE_URL and BUNNY_* from the environment. Flags: -dry-run,
// -limit N, -concurrency N (default 4).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/linear"
)

type pending struct {
	zpid, videoURL, hash string
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report without downloading, segmenting, uploading, or updating")
	limit := flag.Int("limit", 0, "process at most this many videos (0 = all)")
	concurrency := flag.Int("concurrency", 4, "videos processed in parallel")
	flag.Parse()

	if err := run(context.Background(), *dryRun, *limit, *concurrency); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dryRun bool, limit, concurrency int) error {
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	up := bunny.New(
		os.Getenv("BUNNY_STORAGE_ZONE"),
		os.Getenv("BUNNY_API_KEY"),
		envStr("BUNNY_STORAGE_HOST", "storage.bunnycdn.com"),
		os.Getenv("BUNNY_CDN_BASE_URL"),
		300*time.Second,
	)
	repo := linear.NewRepository(pool)
	seg := hls.NewSegmenter()
	httpc := &http.Client{Timeout: 120 * time.Second}

	todo, err := listPending(ctx, pool, limit)
	if err != nil {
		return err
	}
	log.Printf("found %d ready videos without segments", len(todo))
	if dryRun {
		for _, p := range todo {
			log.Printf("zpid=%s: would segment %s", p.zpid, p.videoURL)
		}
		return nil
	}

	var done, failed atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(concurrency, 1))
	for _, p := range todo {
		g.Go(func() error {
			if err := segmentOne(gctx, httpc, seg, up, repo, p); err != nil {
				log.Printf("zpid=%s failed: %v", p.zpid, err)
				failed.Add(1)
				return nil // keep going; the next run retries it
			}
			done.Add(1)
			return nil
		})
	}
	_ = g.Wait()
	log.Printf("done: %d segmented, %d failed", done.Load(), failed.Load())
	return nil
}

// listPending returns ready videos whose current render has no video_hls
// row, oldest first so the backlog drains deterministically.
func listPending(ctx context.Context, pool *pgxpool.Pool, limit int) ([]pending, error) {
	q := `
SELECT p.zpid, p.video_url, COALESCE(p.video_content_hash, '')
  FROM properties p
  LEFT JOIN video_hls h ON h.zpid = p.zpid AND h.content_hash = COALESCE(p.video_content_hash, '')
 WHERE p.video_status = 'ready' AND p.video_url IS NOT NULL AND p.video_url <> '' AND h.id IS NULL
 ORDER BY p.created_at ASC`
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.zpid, &p.videoURL, &p.hash); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func segmentOne(ctx context.Context, httpc *http.Client, seg *hls.Segmenter, up *bunny.Client, repo *linear.Repository, p pending) error {
	workDir, err := os.MkdirTemp("", "backfill-hls-"+p.zpid+"-")
	if err != nil {
		return fmt.Errorf("work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	mp4 := filepath.Join(workDir, "video.mp4")
	if err := download(ctx, httpc, p.videoURL, mp4); err != nil {
		return fmt.Errorf("download %s: %w", p.videoURL, err)
	}
	dir := filepath.Join(workDir, "hls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	clip, err := seg.Segment(ctx, mp4, dir)
	if err != nil {
		return err
	}
	base, err := hls.Upload(ctx, up, dir, hls.Prefix(p.zpid, p.hash), clip, 8)
	if err != nil {
		return err
	}
	if err := repo.SetVideoHLS(ctx, p.zpid, p.hash, base, clip); err != nil {
		return err
	}
	log.Printf("zpid=%s: %d segments, %d ms → %s", p.zpid, len(clip.SegmentMS), clip.TotalMS, base)
	return nil
}

func download(ctx context.Context, c *http.Client, src, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, res.Body)
	return err
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Build and dry-run against nothing**

Run: `go build ./... && go vet ./cmd/backfill-hls/`
Expected: clean. (A real dry run needs `DATABASE_URL`; the operator runs `go run ./cmd/backfill-hls -dry-run` against prod after deploy.)

- [ ] **Step 3: Commit**

```bash
gofmt -l .
git add cmd/backfill-hls/
git commit -m "feat: backfill-hls segments the existing video library"
```

---

### Task 11: config, server mounting, Roku liveFeeds, main wiring

**Files:**
- Modify: `internal/config/config.go` (add `Linear`, `PublicBaseURL`)
- Modify: `internal/config/config_test.go` (defaults test)
- Modify: `internal/feed/feed.go` (`LiveFeed`, `Feed.LiveFeeds`, `AddLive`)
- Modify: `internal/feed/feed_test.go` (liveFeeds test)
- Modify: `internal/server/server.go` (`mux` field, `Mount`, `SetLiveFeedURL`, feed live entry)
- Modify: `internal/server/server_test.go` (mount + live feed tests)
- Modify: `cmd/server/main.go` (wire linear repo/service/handler, `EnableHLS`)

**Interfaces:**
- Produces: `config.LinearConfig{Enabled bool; LineupHours, MinScopeClips, EPGHorizonHours int}`, `Config.Linear`, `Config.PublicBaseURL`; `feed.LiveFeed`, `feed.LiveContent`, `(*feed.Feed).AddLive(streamURL, thumbnail string, now time.Time)`; `server.Mounter` interface, `(*server.Server).Mount(Mounter)`, `(*server.Server).SetLiveFeedURL(string)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (check the file's existing helper for setting env; use `t.Setenv`):

```go
func TestLoad_LinearDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("ZILLOW_API_KEY", "k")
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("PUBLIC_BASE_URL", "https://api.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Linear.Enabled || cfg.Linear.LineupHours != 6 || cfg.Linear.MinScopeClips != 20 || cfg.Linear.EPGHorizonHours != 24 {
		t.Errorf("linear defaults = %+v", cfg.Linear)
	}
	if cfg.PublicBaseURL != "https://api.example.com" {
		t.Errorf("PublicBaseURL = %q (trailing slash must be trimmed)", cfg.PublicBaseURL)
	}
}
```

Append to `internal/feed/feed_test.go`:

```go
func TestAddLive_AppendsRokuLiveFeed(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	f := Build("DwellingTV", nil, now)
	f.AddLive("https://api.example.com/channels/master.m3u8", "https://cdn/thumb.jpg", now)

	if len(f.LiveFeeds) != 1 {
		t.Fatalf("liveFeeds = %d, want 1", len(f.LiveFeeds))
	}
	lf := f.LiveFeeds[0]
	if lf.ID != "dwellingtv-live" || lf.Title != "DwellingTV Live" || lf.Thumbnail != "https://cdn/thumb.jpg" {
		t.Errorf("live feed = %+v", lf)
	}
	if lf.Content.DateAdded != "2026-08-25T12:00:00Z" {
		t.Errorf("dateAdded = %s", lf.Content.DateAdded)
	}
	v := lf.Content.Videos[0]
	if v.URL != "https://api.example.com/channels/master.m3u8" || v.VideoType != "HLS" || v.Quality != "HD" {
		t.Errorf("video = %+v", v)
	}
	out, _ := json.Marshal(f)
	if !strings.Contains(string(out), `"liveFeeds":[{"id":"dwellingtv-live"`) {
		t.Errorf("json = %s", out)
	}
	// Without AddLive the key is omitted entirely (Roku rejects empty arrays).
	out, _ = json.Marshal(Build("DwellingTV", nil, now))
	if strings.Contains(string(out), "liveFeeds") {
		t.Errorf("json without live feed must omit liveFeeds: %s", out)
	}
}
```

(Add `"strings"` to the feed test imports.)

Append to `internal/server/server_test.go`:

```go
type stubMounter struct{}

func (stubMounter) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /stub", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
}

func TestMount_RegistersRoutes(t *testing.T) {
	s := New(":0", "Dwellings", nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Mount(stubMounter{})
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stub", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("GET /stub = %d %q", rec.Code, rec.Body.String())
	}
}

type emptyFeed struct{}

func (emptyFeed) ListReadyForFeed(context.Context) ([]property.Property, error) { return nil, nil }

func TestFeed_IncludesLiveFeedWhenConfigured(t *testing.T) {
	s := New(":0", "Dwellings", emptyFeed{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetLiveFeedURL("https://api.example.com/channels/master.m3u8")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"liveFeeds"`) || !strings.Contains(rec.Body.String(), "channels/master.m3u8") {
		t.Errorf("feed lacks live entry:\n%s", rec.Body.String())
	}
}
```

(Add `"context"` and `"github.com/dwellingtw/backend/internal/property"` to the server test imports.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ ./internal/feed/ ./internal/server/ 2>&1 | head -20`
Expected: build failures — `Linear`, `PublicBaseURL`, `AddLive`, `Mount`, `SetLiveFeedURL` undefined.

- [ ] **Step 3: Config**

In `internal/config/config.go`, add to `Config` after `Video VideoConfig`:

```go
	// Linear channels — 24/7 HLS streams assembled from the listing videos.
	Linear LinearConfig

	// PublicBaseURL is this server's public origin (e.g. https://api.dwellings.tv)
	// for absolute URLs in the Roku feed. Empty disables the feed's live entry.
	PublicBaseURL string
```

Add the type after `VideoConfig`:

```go
// LinearConfig controls the linear channels.
type LinearConfig struct {
	Enabled         bool
	LineupHours     int // max content per lineup version
	MinScopeClips   int // a scope with fewer clips falls back to its parent area
	EPGHorizonHours int // how far ahead the EPG materialises the schedule
}
```

In `Load`, after the `Video: VideoConfig{…},` literal add:

```go
		Linear: LinearConfig{
			Enabled:         getenvBool("LINEAR_ENABLED", true),
			LineupHours:     getenvInt("LINEAR_LINEUP_HOURS", 6),
			MinScopeClips:   getenvInt("LINEAR_MIN_SCOPE_CLIPS", 20),
			EPGHorizonHours: getenvInt("LINEAR_EPG_HORIZON_HOURS", 24),
		},
		PublicBaseURL: strings.TrimRight(getenv("PUBLIC_BASE_URL", ""), "/"),
```

- [ ] **Step 4: Feed**

In `internal/feed/feed.go`, add to `Feed`:

```go
	LiveFeeds       []LiveFeed       `json:"liveFeeds,omitempty"`
```

and the types + method:

```go
// LiveFeed is a Roku Direct Publisher liveFeeds entry: a linear stream.
type LiveFeed struct {
	ID               string      `json:"id"`
	Title            string      `json:"title"`
	ShortDescription string      `json:"shortDescription"`
	Thumbnail        string      `json:"thumbnail"`
	Content          LiveContent `json:"content"`
	Tags             []string    `json:"tags,omitempty"`
}

// LiveContent holds the live stream URL.
type LiveContent struct {
	DateAdded string  `json:"dateAdded"`
	Videos    []Video `json:"videos"`
}

// AddLive appends the national linear channel as a live feed.
func (f *Feed) AddLive(streamURL, thumbnail string, now time.Time) {
	f.LiveFeeds = append(f.LiveFeeds, LiveFeed{
		ID:               "dwellingtv-live",
		Title:            "DwellingTV Live",
		ShortDescription: "Homes for sale, around the clock",
		Thumbnail:        thumbnail,
		Content: LiveContent{
			DateAdded: now.UTC().Format(time.RFC3339),
			Videos:    []Video{{URL: streamURL, Quality: "HD", VideoType: "HLS"}},
		},
		Tags: []string{"real estate"},
	})
}
```

- [ ] **Step 5: Server**

In `internal/server/server.go`:

Add fields to `Server`:

```go
	mux          *http.ServeMux
	liveURL      string
```

In `New`, keep the mux on the struct (`s.mux = mux` right after `mux := http.NewServeMux()`).

Add:

```go
// Mounter registers routes on a mux (e.g. linear.Handler).
type Mounter interface {
	Register(mux *http.ServeMux)
}

// Mount adds routes. Call before Start.
func (s *Server) Mount(m Mounter) { m.Register(s.mux) }

// SetLiveFeedURL adds the linear channel to the Roku feed as a live feed.
func (s *Server) SetLiveFeedURL(u string) { s.liveURL = u }
```

In `handleFeed`, after `doc := feed.Build(...)`:

```go
	if s.liveURL != "" {
		thumb := ""
		if len(props) > 0 && len(props[0].ImageURLs) > 0 {
			thumb = props[0].ImageURLs[0]
		}
		doc.AddLive(s.liveURL, thumb, s.now())
	}
```

- [ ] **Step 6: main wiring**

In `cmd/server/main.go`, add imports `"github.com/dwellingtw/backend/internal/hls"` and `"github.com/dwellingtw/backend/internal/linear"`. Replace the block from `zipRepo := zipcode.NewRepository(pool)` through the `go func() { … httpSrv.Start() … }()` with:

```go
	zipRepo := zipcode.NewRepository(pool)
	sched := scheduler.New(cfg, zillowClient, bunnyClient, repo, zipRepo, rendererOrNil(renderer), log)

	// Linear channels: segment new renders and serve the channel endpoints.
	var linearRepo *linear.Repository
	if cfg.Linear.Enabled {
		linearRepo = linear.NewRepository(pool)
		if cfg.Video.Enabled {
			sched.EnableHLS(hls.NewSegmenter(), linearRepo)
		}
	}

	if err := sched.Start(ctx); err != nil {
		return err
	}
	log.Info("scheduler started", "schedule", cfg.CronSchedule)

	publicAPI := api.New(repo, mapEnsurerOrNil(mapSvc), log)
	httpSrv := server.New(net.JoinHostPort("", cfg.HTTPPort), "DwellingTV", repo, publicAPI, log)
	if linearRepo != nil {
		svc := linear.New(linearRepo, linear.Options{
			LineupHours:     cfg.Linear.LineupHours,
			MinScopeClips:   cfg.Linear.MinScopeClips,
			EPGHorizonHours: cfg.Linear.EPGHorizonHours,
		}, log)
		httpSrv.Mount(linear.NewHandler(svc, log))
		if cfg.PublicBaseURL != "" {
			httpSrv.SetLiveFeedURL(cfg.PublicBaseURL + "/channels/master.m3u8")
		}
		log.Info("linear channels enabled", "lineup_hours", cfg.Linear.LineupHours, "min_scope_clips", cfg.Linear.MinScopeClips)
	}
	go func() {
		log.Info("http server started", "port", cfg.HTTPPort)
		if err := httpSrv.Start(); err != nil {
			log.Error("http server stopped", "error", err)
		}
	}()
```

- [ ] **Step 7: Run everything**

Run: `gofmt -l . ; go vet ./... && go test ./... -race`
Expected: gofmt prints nothing; vet clean; all packages PASS (ffmpeg integration tests pass locally with Homebrew ffmpeg).

- [ ] **Step 8: Commit**

```bash
git add internal/config/ internal/feed/ internal/server/ cmd/server/main.go
git commit -m "feat: wire linear channels into config, server and Roku feed"
```

---

### Task 12: README

**Files:**
- Modify: `README.md` (architecture list, new section after "Property maps", endpoints list, config table if present)

- [ ] **Step 1: Document**

In the architecture block add the lines:

```
internal/hls            ffmpeg remux of a listing video into TS segments
internal/linear         24/7 linear channels: lineups, live HLS playlist, EPG
```

After the "### Property maps" section add:

````markdown
### Linear channels

The listing videos double as 24/7 "TV channels": a live HLS stream plus an
EPG, with no running encoder. Every rendered MP4 is remuxed once into TS
segments (`hls/v1/<zpid>/<hash8>/` on Bunny CDN, immutable) and a channel is a
deterministic lineup materialised in `channel_lineups` in 6-hour versions.
The playlist a viewer fetches is computed from the wall clock, so two
instances — or a restart mid-stream — serve byte-identical playlists.

```
GET /channels/master.m3u8[?zip=77494 | ?city=Katy&state=TX | ?state=TX]
GET /channels/live.m3u8   (same filters; the master playlist points here)
GET /channels/epg.json    (same filters; 30-minute programme blocks)
```

No filter is the national channel. A ZIP with fewer than
`LINEAR_MIN_SCOPE_CLIPS` (20) videos falls back to its city, then state, then
national; `epg.json` reports the `scope` actually aired. New listings enter
the rotation at the next lineup version (at most `LINEAR_LINEUP_HOURS` later).

Segments for new renders are produced by the scheduler. To segment the
existing library (and retry failures):

```bash
go run ./cmd/backfill-hls -dry-run
go run ./cmd/backfill-hls -concurrency 4
```

With `PUBLIC_BASE_URL` set, `/roku/feed.json` also lists the national channel
as a Roku `liveFeeds` entry. Set `LINEAR_ENABLED=false` to turn all of this
off.
````

In the HTTP endpoints list add:

```markdown
- `GET /channels/master.m3u8`, `GET /channels/live.m3u8`, `GET /channels/epg.json` — linear channels, see [Linear channels](#linear-channels).
```

If the README has an environment-variable table, add rows for `LINEAR_ENABLED` (`true`), `LINEAR_LINEUP_HOURS` (`6`), `LINEAR_MIN_SCOPE_CLIPS` (`20`), `LINEAR_EPG_HORIZON_HOURS` (`24`), `PUBLIC_BASE_URL` (empty).

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: linear channels"
```

---

## Self-review

- **Spec coverage:** §1 segmentation → Task 1; storage prefix/immutability → Task 2; §2 tables → Task 3; §3 scope + fallback → Tasks 3–4; §4 lineup chain (seed, cap, historyLead, idle restart, ON CONFLICT) → Task 4; §5 playlist rules (window, counters, discontinuity, PDT, headers, master) → Tasks 5–6; §6 EPG → Task 7; §7 Roku liveFeeds → Task 11; §8 scheduler + backfill → Tasks 9–10; §9 config → Task 11; error handling (400/503/500) → Task 6; README → Task 12.
- **Type consistency:** `hls.Clip{SegmentMS, TotalMS}` used identically in Tasks 1, 2, 8, 9, 10; `Version` fields match between store.go, repository.go and timeline.go; `Store` methods match `memStore` and `Repository`; `EnableHLS(Segmenter, HLSRecorder)` matches `main.go` (`*hls.Segmenter`, `*linear.Repository`).
- **Deliberate deviations from the spec:** none.
