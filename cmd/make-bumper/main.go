// Command make-bumper renders the QR bumper (internal/bumper), segments it
// for HLS, uploads it under hls/bumpers/<hash8>/ and registers it in the
// bumpers table. With -kind slate it renders the ad-break slate instead
// (its own caption and length, uploaded under hls/slates/<hash8>/, kind
// 'slate' in the table): what airs while the playlist is cued out for SSAI.
// The newest row of a kind is what new lineup versions air, so running this
// is how either clip is changed. Rerunning with the same inputs is a no-op
// (the content hash exists) and prints the existing id.
//
// Reads DATABASE_URL, BUNNY_*, VIDEO_FONT_PATH and VIDEO_FPS from the
// environment. With -render-only PATH it only writes the MP4 there (no
// database, no upload): handy to eyeball the result locally.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/dwellingtw/backend/internal/bumper"
	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/linear"
)

// toolMaxConns keeps the run small next to the fleet's pools.
const toolMaxConns = 2

func main() {
	spec := bumper.Spec{FPS: config.MinVideoFPS}
	kind := flag.String("kind", linear.KindBumper, "what to make: bumper (the QR invitation) or slate (the ad-break filler)")
	flag.StringVar(&spec.URL, "url", bumper.DefaultURL, "URL the QR encodes")
	flag.StringVar(&spec.Caption, "caption", bumper.DefaultCaption, `caption; "\n" separates lines (a slate defaults to its own)`)
	flag.IntVar(&spec.Seconds, "seconds", bumper.DefaultSeconds, "length in seconds: bumper 3..60, slate 3..180 (default 60)")
	flag.StringVar(&spec.Background, "bg", bumper.DefaultColor, "background colour (ffmpeg syntax)")
	flag.StringVar(&spec.BackgroundImage, "bg-image", "", "background image (16:9), overrides -bg")
	flag.StringVar(&spec.Music, "music", "", "music file, e.g. assets/music/03-calm-piano.mp3 (silent without)")
	name := flag.String("name", "default", "label stored with the row")
	renderOnly := flag.String("render-only", "", "write the MP4 here and stop (no upload, no database)")
	uploadConcurrency := flag.Int("upload-concurrency", 8, "segment uploads in parallel")
	flag.Parse()
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if err := applyKind(&spec, *kind, set["caption"], set["seconds"]); err != nil {
		log.Fatal(err)
	}
	spec.FontPath = envStr("VIDEO_FONT_PATH", "/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf")
	if v, err := strconv.Atoi(os.Getenv("VIDEO_FPS")); err == nil && v > 0 {
		spec.FPS = v
	}
	spec.Caption = unescapeNewlines(spec.Caption)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, spec, *name, *renderOnly, *uploadConcurrency); err != nil {
		log.Fatal(err)
	}
}

// applyKind sets spec.Kind and, for a slate whose -caption or -seconds were
// not given, the slate defaults; then checks the length against the kind's
// range (a bumper is an interruption, a slate a whole ad break).
func applyKind(spec *bumper.Spec, kind string, captionSet, secondsSet bool) error {
	maxSeconds := 60
	switch kind {
	case linear.KindBumper:
	case linear.KindSlate:
		maxSeconds = 180
		if !captionSet {
			spec.Caption = bumper.SlateCaption
		}
		if !secondsSet {
			spec.Seconds = bumper.SlateSeconds
		}
	default:
		return fmt.Errorf("-kind must be %s or %s, got %q", linear.KindBumper, linear.KindSlate, kind)
	}
	spec.Kind = kind
	if spec.Seconds < 3 || spec.Seconds > maxSeconds {
		return fmt.Errorf("-seconds must be 3..%d for a %s, got %d", maxSeconds, kind, spec.Seconds)
	}
	return nil
}

func run(ctx context.Context, spec bumper.Spec, name, renderOnly string, uploadConcurrency int) error {
	if spec.FPS < config.MinVideoFPS {
		spec.FPS = config.MinVideoFPS
	}
	workDir, err := os.MkdirTemp("", "make-bumper-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	if renderOnly != "" {
		mp4, err := bumper.Render(ctx, spec, workDir)
		if err != nil {
			return err
		}
		if err := moveFile(mp4, renderOnly); err != nil {
			return err
		}
		log.Printf("rendered %s", renderOnly)
		return nil
	}

	hash, err := spec.Hash()
	if err != nil {
		return fmt.Errorf("hash inputs: %w", err)
	}
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"), toolMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	repo := linear.NewRepository(pool)
	if existing, err := repo.BumperByHash(ctx, hash); err != nil {
		return err
	} else if existing != nil {
		log.Printf("%s with these inputs already exists: id=%d name=%s created=%s", spec.Kind, existing.ID, existing.Name, existing.CreatedAt.Format(time.RFC3339))
		fmt.Println(existing.ID)
		return nil
	}

	mp4, err := bumper.Render(ctx, spec, workDir)
	if err != nil {
		return err
	}
	dir := filepath.Join(workDir, "hls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	clip, err := hls.NewSegmenter().Segment(ctx, mp4, dir)
	if err != nil {
		return err
	}
	up := bunny.New(os.Getenv("BUNNY_STORAGE_ZONE"), os.Getenv("BUNNY_API_KEY"),
		envStr("BUNNY_STORAGE_HOST", "storage.bunnycdn.com"), os.Getenv("BUNNY_CDN_BASE_URL"), 300*time.Second)
	prefix := hls.BumperPrefix(hash)
	if spec.Kind == linear.KindSlate {
		prefix = hls.SlatePrefix(hash)
	}
	base, err := hls.Upload(ctx, up, dir, prefix, clip, max(uploadConcurrency, 1))
	if err != nil {
		return err
	}
	id, created, err := repo.InsertBumper(ctx, &linear.Bumper{Kind: spec.Kind, Name: name, ContentHash: hash, BaseURL: base, SegmentMS: clip.SegmentMS, TotalMS: clip.TotalMS})
	if err != nil {
		return err
	}
	log.Printf("%s id=%d created=%v: %d segments, %d ms → %s", spec.Kind, id, created, len(clip.SegmentMS), clip.TotalMS, base)
	log.Printf("new lineup versions carry it within LINEAR_LINEUP_HOURS")
	fmt.Println(id)
	return nil
}

// moveFile renames src to dest, copying when they are on different
// filesystems (the work dir is in the system temp dir).
func moveFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o644)
}

// unescapeNewlines turns a literal backslash-n (as typed on a command line)
// into a newline, so -caption can carry several lines.
func unescapeNewlines(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == 'n' {
			out = append(out, '\n')
			i++
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
