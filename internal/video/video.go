// Package video renders a 16:9 1080p slideshow MP4 for a listing using ffmpeg:
// photos scaled-to-cover, a persistent lower-third facts overlay, a QR code to
// the Zillow listing, and a background music track.
package video

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/qrcode"
)

// Renderer turns a property + local photos into an MP4.
type Renderer struct {
	secondsPerPhoto int
	fontPath        string
	musicTracks     []string // sorted absolute paths; may be empty
	ffmpeg          string
	threads         int // per-render thread cap; 0 = ffmpeg decides
	fps             int // frames per second; 0 = defaultFPS
}

// New creates a Renderer, loading the available music tracks from the configured
// directory (sorted for deterministic selection).
func New(cfg config.VideoConfig) (*Renderer, error) {
	tracks, err := loadTracks(cfg.MusicDir)
	if err != nil {
		return nil, err
	}
	secs := cfg.SecondsPerPhoto
	if secs <= 0 {
		secs = 4
	}
	return &Renderer{
		secondsPerPhoto: secs,
		fontPath:        cfg.FontPath,
		musicTracks:     tracks,
		ffmpeg:          "ffmpeg",
		threads:         cfg.Threads,
		fps:             cfg.FPS,
	}, nil
}

func loadTracks(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no music dir → silent videos
		}
		return nil, fmt.Errorf("read music dir %s: %w", dir, err)
	}
	var tracks []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".mp3") {
			abs, _ := filepath.Abs(filepath.Join(dir, e.Name()))
			tracks = append(tracks, abs)
		}
	}
	sort.Strings(tracks)
	return tracks, nil
}

// TrackCount reports how many music tracks were loaded.
func (r *Renderer) TrackCount() int { return len(r.musicTracks) }

// selectMusic picks a track deterministically by zpid so re-renders of the same
// listing keep the same music. Returns "" if no tracks are available.
func (r *Renderer) selectMusic(zpid string) string {
	if len(r.musicTracks) == 0 {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(zpid))
	return r.musicTracks[int(h.Sum32())%len(r.musicTracks)]
}

// ContentHash captures the inputs that affect a rendered video, so the scheduler
// can skip re-rendering unchanged listings.
func ContentHash(p *property.Property, secondsPerPhoto, fps int) string {
	h := sha256.New()
	fmt.Fprintf(h, "v2|%d|%d|%s|%s|%s|%d|%s|", secondsPerPhoto, fpsOrDefault(fps), p.ZPID, priceText(p), addressText(p), p.SalePrice, factsText(p))
	fmt.Fprintf(h, "%s|", p.DetailURL)
	for _, u := range p.ImageURLs {
		fmt.Fprintf(h, "%s,", u)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Render builds the MP4 at outPath from the given local image files. It returns
// the video duration in seconds. workDir is used for scratch files (text/QR) and
// must already exist; the caller owns its lifecycle.
func (r *Renderer) Render(ctx context.Context, p *property.Property, imagePaths []string, workDir, outPath string) (int, error) {
	if len(imagePaths) == 0 {
		return 0, fmt.Errorf("render zpid=%s: no images", p.ZPID)
	}

	priceFile := filepath.Join(workDir, "price.txt")
	addrFile := filepath.Join(workDir, "address.txt")
	factsFile := filepath.Join(workDir, "facts.txt")
	for path, text := range map[string]string{
		priceFile: priceText(p),
		addrFile:  addressText(p),
		factsFile: factsText(p),
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			return 0, fmt.Errorf("write overlay text: %w", err)
		}
	}

	qrPath := ""
	if p.DetailURL != "" {
		qrPath = filepath.Join(workDir, "qr.png")
		if err := qrcode.WritePNG(p.DetailURL, qrPath, 600); err != nil {
			return 0, err
		}
	}

	spec := renderSpec{
		imagePaths:      imagePaths,
		secondsPerPhoto: r.secondsPerPhoto,
		fontPath:        r.fontPath,
		priceFile:       priceFile,
		addressFile:     addrFile,
		factsFile:       factsFile,
		caption:         "Scan for details",
		qrPath:          qrPath,
		musicPath:       r.selectMusic(p.ZPID),
		outPath:         outPath,
		threads:         r.threads,
		fps:             r.fps,
	}
	args := buildFFmpegArgs(spec)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	// A killed ffmpeg must not leave Run blocked on its stderr pipe: that would hold a work slot past the claim's lease.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffmpeg zpid=%s: %w: %s", p.ZPID, err, tail(stderr.String(), 600))
	}

	return len(imagePaths) * r.secondsPerPhoto, nil
}

// renderSpec is the fully-resolved input to the ffmpeg args builder.
type renderSpec struct {
	imagePaths      []string
	secondsPerPhoto int
	fontPath        string
	priceFile       string
	addressFile     string
	factsFile       string
	caption         string
	qrPath          string // "" to skip
	musicPath       string // "" to skip
	outPath         string
	// fps is the frame rate of the finished video. 0 means defaultFPS.
	//
	// The listings are still photographs under a static overlay, so every
	// frame after the first of each photo is a duplicate and the frame rate
	// buys throughput almost for free: measured on a production box (29
	// photos, 145 s of video, one CPU) 30 fps took 97 s and 11 MB while 15 fps
	// took 63 s and 12 MB.
	fps int
	// threads caps every pool one render may open: each image decoder, the
	// filter graph and x264. 0 leaves ffmpeg's own sizing alone.
	//
	// It matters because ffmpeg sizes those pools from the CORE COUNT IT SEES,
	// not from how many renders share the machine. A box running one render
	// per core therefore opened ~700 threads per process — 25,000 on 32 cores
	// — and lost more to context switching than it gained in parallelism.
	threads int
}

// buildFFmpegArgs constructs the ffmpeg argument list. Pure function — no I/O —
// so it can be asserted in tests without running ffmpeg.
func buildFFmpegArgs(s renderSpec) []string {
	n := len(s.imagePaths)
	secs := strconv.Itoa(s.secondsPerPhoto)
	fps := fpsOrDefault(s.fps)

	args := []string{"-y"}
	threads := ""
	if s.threads > 0 {
		threads = strconv.Itoa(s.threads)
		// Global: the filter graph would otherwise open a pool per core.
		args = append(args, "-filter_threads", threads, "-filter_complex_threads", threads)
	}
	for _, img := range s.imagePaths {
		if threads != "" {
			// Per input: one decoder pool per photo, and a listing has ~29.
			args = append(args, "-threads", threads)
		}
		args = append(args, "-loop", "1", "-t", secs, "-i", img)
	}
	musicIdx, qrIdx := -1, -1
	if s.musicPath != "" {
		musicIdx = n
		args = append(args, "-stream_loop", "-1", "-i", s.musicPath)
	}
	if s.qrPath != "" {
		qrIdx = n
		if musicIdx >= 0 {
			qrIdx = n + 1
		}
		args = append(args, "-i", s.qrPath)
	}

	args = append(args, "-filter_complex", buildFilterGraph(s, qrIdx))

	finalLabel := "[ov]"
	if s.qrPath != "" {
		finalLabel = "[outv]"
	}
	args = append(args, "-map", finalLabel)

	if musicIdx >= 0 {
		args = append(args,
			"-map", fmt.Sprintf("%d:a", musicIdx),
			"-c:a", "aac", "-b:a", "160k",
			"-af", "volume=0.5,afade=t=in:d=1",
			"-shortest",
		)
	}

	args = append(args,
		"-r", strconv.Itoa(fps),
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "veryfast", "-crf", "20",
		// The content is stills: this tuning is measurably cheaper (91 s
		// against 97 s on the benchmark above) at the same file size.
		"-tune", "stillimage",
		"-movflags", "+faststart",
	)
	if threads != "" {
		args = append(args, "-threads", threads) // x264
	}
	args = append(args, s.outPath)
	return args
}

func buildFilterGraph(s renderSpec, qrIdx int) string {
	n := len(s.imagePaths)
	fps := fpsOrDefault(s.fps)
	var parts []string

	// 1) Each photo: scale-to-cover, center-crop to 16:9, normalize.
	var concatInputs strings.Builder
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf(
			"[%d:v]scale=1920:1080:force_original_aspect_ratio=increase,crop=1920:1080,setsar=1,fps=%d,format=yuv420p[v%d]",
			i, fps, i))
		fmt.Fprintf(&concatInputs, "[v%d]", i)
	}
	parts = append(parts, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[slide]", concatInputs.String(), n))

	// 2) Lower-third bar + persistent facts overlays.
	overlay := "[slide]drawbox=x=0:y=ih-240:w=iw:h=240:color=black@0.5:t=fill"
	if s.priceFile != "" {
		overlay += fmt.Sprintf(",drawtext=fontfile='%s':textfile='%s':fontsize=72:fontcolor=white:x=60:y=h-220", s.fontPath, s.priceFile)
	}
	if s.addressFile != "" {
		overlay += fmt.Sprintf(",drawtext=fontfile='%s':textfile='%s':fontsize=38:fontcolor=white:x=60:y=h-135", s.fontPath, s.addressFile)
	}
	if s.factsFile != "" {
		overlay += fmt.Sprintf(",drawtext=fontfile='%s':textfile='%s':fontsize=32:fontcolor=0xDDDDDD:x=60:y=h-80", s.fontPath, s.factsFile)
	}
	overlay += "[ov]"
	parts = append(parts, overlay)

	// 3) Composite the QR code bottom-right on a solid white card (so it scans
	//    reliably over bright photos), with the caption as dark text on the card.
	if s.qrPath != "" {
		qr := fmt.Sprintf("[%d:v]scale=200:200,pad=224:264:12:12:color=white", qrIdx)
		if s.caption != "" {
			qr += fmt.Sprintf(",drawtext=fontfile='%s':text='%s':fontsize=22:fontcolor=black:x=(w-tw)/2:y=224", s.fontPath, s.caption)
		}
		qr += "[qr]"
		parts = append(parts, qr)
		parts = append(parts, "[ov][qr]overlay=W-w-60:H-h-60[outv]")
	}

	return strings.Join(parts, ";")
}

// defaultFPS is what every listing was rendered at before the frame rate
// became configurable; keeping it as the fallback lets the knob ship dark.
const defaultFPS = 30

func fpsOrDefault(fps int) int {
	if fps <= 0 {
		return defaultFPS
	}
	return fps
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
