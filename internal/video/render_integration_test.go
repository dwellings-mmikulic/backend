package video

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/config"
)

// fontCandidates covers Linux (CI/Docker) and macOS (local dev).
var fontCandidates = []string{
	"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
	"/System/Library/Fonts/Supplemental/Arial Bold.ttf",
	"/System/Library/Fonts/Supplemental/Arial.ttf",
	"/Library/Fonts/Arial.ttf",
}

func findFont() string {
	for _, f := range fontCandidates {
		if _, err := os.Stat(f); err == nil {
			return f
		}
	}
	return ""
}

func ffmpegHasFilter(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), " "+name+" ")
}

func writeJPEG(t *testing.T, path string, w, h int, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
}

func TestRender_RealFFmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ffmpeg integration test in -short mode")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	if !ffmpegHasFilter(t, "drawtext") {
		t.Skip("ffmpeg built without drawtext filter (needs libfreetype); verified in Docker instead")
	}
	font := findFont()
	if font == "" {
		t.Skip("no usable TTF font found")
	}

	work := t.TempDir()
	// Two photos of different sizes to exercise scale-to-cover + crop.
	img0 := filepath.Join(work, "0.jpg")
	img1 := filepath.Join(work, "1.jpg")
	writeJPEG(t, img0, 1600, 1200, color.RGBA{40, 90, 160, 255}) // 4:3
	writeJPEG(t, img1, 1920, 1280, color.RGBA{160, 90, 40, 255}) // 3:2

	r, err := New(config.VideoConfig{
		Enabled:         true,
		SecondsPerPhoto: 1,
		MusicDir:        "../../assets/music",
		FontPath:        font,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.musicTracks) == 0 {
		t.Log("warning: no music tracks found; rendering silent video")
	}

	out := filepath.Join(work, "out.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dur, err := r.Render(ctx, sampleProperty(), []string{img0, img1}, work, out)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if dur != 2 {
		t.Errorf("duration = %d, want 2", dur)
	}

	info := ffprobe(t, out)
	var vStream, aStream *probeStream
	for i := range info.Streams {
		switch info.Streams[i].CodecType {
		case "video":
			vStream = &info.Streams[i]
		case "audio":
			aStream = &info.Streams[i]
		}
	}
	if vStream == nil {
		t.Fatal("no video stream in output")
	}
	if vStream.Width != 1920 || vStream.Height != 1080 {
		t.Errorf("resolution = %dx%d, want 1920x1080", vStream.Width, vStream.Height)
	}
	if vStream.CodecName != "h264" {
		t.Errorf("codec = %s, want h264", vStream.CodecName)
	}
	if len(r.musicTracks) > 0 && aStream == nil {
		t.Error("expected an audio stream (music tracks present) but found none")
	}
	t.Logf("rendered %s: %dx%d %s, audio=%v", out, vStream.Width, vStream.Height, vStream.CodecName, aStream != nil)
}

type probeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type probeResult struct {
	Streams []probeStream `json:"streams"`
}

func ffprobe(t *testing.T, path string) probeResult {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var r probeResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("parse ffprobe json: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return r
}
