package bumper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildFFmpegArgs_ProfileMatchesListings(t *testing.T) {
	s := Spec{URL: DefaultURL, Caption: DefaultCaption, Seconds: 12, Background: "0x101418", FontPath: "/f.ttf", FPS: 30}
	args := buildFFmpegArgs(s, "/w/qr.png", []string{"/w/c0.txt", "/w/c1.txt"}, "/w/url.txt", "/w/out.mp4")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-f lavfi -i color=c=0x101418:s=1920x1080:r=30:d=12",
		"-i /w/qr.png",
		"-f lavfi -i anullsrc=r=48000:cl=stereo",
		"-c:v libx264 -pix_fmt yuv420p -preset veryfast -crf 20",
		"-g 90 -keyint_min 90 -sc_threshold 0",
		"-c:a aac -b:a 160k",
		"-r 30", "-t 12", "-movflags +faststart",
		"textfile='/w/c0.txt'", "textfile='/w/c1.txt'", "textfile='/w/url.txt'", "fontfile='/f.ttf'",
		"fade=t=out:st=11.5:d=0.5",
		"-map [outv] -map 2:a",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q:\n%s", want, joined)
		}
	}
	if args[len(args)-1] != "/w/out.mp4" {
		t.Errorf("last arg = %q", args[len(args)-1])
	}
}

func TestBuildFFmpegArgs_ImageAndMusic(t *testing.T) {
	s := Spec{URL: DefaultURL, Caption: "one line", Seconds: 10, BackgroundImage: "/w/bg.jpg", Music: "/m.mp3", FontPath: "/f.ttf", FPS: 30}
	joined := strings.Join(buildFFmpegArgs(s, "/w/qr.png", []string{"/w/c0.txt"}, "/w/url.txt", "/w/out.mp4"), " ")
	for _, want := range []string{
		"-loop 1 -t 10 -i /w/bg.jpg",
		"scale=1920:1080:force_original_aspect_ratio=increase,crop=1920:1080",
		"-stream_loop -1 -i /m.mp3",
		"afade=t=in:d=1,afade=t=out:st=9:d=1",
		"-shortest",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "anullsrc") || strings.Contains(joined, "color=c=") {
		t.Errorf("silent/colour inputs present with image and music:\n%s", joined)
	}
}

func TestHash_DependsOnEveryInput(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "bg.png")
	_ = os.WriteFile(img, []byte("img1"), 0o644)
	base := Spec{URL: DefaultURL, Caption: DefaultCaption, Seconds: 12, Background: "0x101418", FontPath: "/f.ttf", FPS: 30}
	h0, err := base.Hash()
	if err != nil || len(h0) != 64 {
		t.Fatalf("hash = %q err=%v", h0, err)
	}
	if h, _ := base.Hash(); h != h0 {
		t.Error("hash is not deterministic")
	}
	vary := []Spec{base, base, base, base, base}
	vary[0].URL = "https://example.test/"
	vary[1].Caption = "x"
	vary[2].Seconds = 13
	vary[3].Background = "0x000000"
	vary[4].BackgroundImage = img
	seen := map[string]bool{h0: true}
	for i, v := range vary {
		h, err := v.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if seen[h] {
			t.Errorf("variant %d collides", i)
		}
		seen[h] = true
	}
	_ = os.WriteFile(img, []byte("img2"), 0o644)
	if h, _ := vary[4].Hash(); seen[h] {
		t.Error("changing the image bytes did not change the hash")
	}
	missing := base
	missing.Music = filepath.Join(dir, "nope.mp3")
	if _, err := missing.Hash(); err == nil {
		t.Error("missing music file must fail the hash")
	}
}

func TestRender_WritesAnMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	// drawtext needs an ffmpeg built with libfreetype (the Alpine one in the
	// image has it; a Homebrew build may not).
	if out, _ := exec.Command("ffmpeg", "-hide_banner", "-filters").Output(); !strings.Contains(string(out), " drawtext ") {
		t.Skip("ffmpeg lacks the drawtext filter")
	}
	font := os.Getenv("VIDEO_FONT_PATH")
	if font == "" {
		for _, p := range []string{"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf", "/System/Library/Fonts/Supplemental/Arial Bold.ttf", "/Library/Fonts/Arial Bold.ttf"} {
			if _, err := os.Stat(p); err == nil {
				font = p
				break
			}
		}
	}
	if font == "" {
		t.Skip("no font found; set VIDEO_FONT_PATH")
	}
	s := Spec{URL: DefaultURL, Caption: DefaultCaption, Seconds: 2, Background: "0x101418", FontPath: font, FPS: 30}
	out, err := Render(context.Background(), s, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() < 10000 {
		t.Fatalf("output %s: %v size=%d", out, err, st.Size())
	}
}
