// Package bumper renders the QR bumper: a short 1080p clip with a caption,
// a large QR code and the URL it encodes, in the same encode profile as the
// listing videos so it splices into a channel (see
// docs/superpowers/specs/2026-10-13-qr-bumper-design.md).
package bumper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dwellingtw/backend/internal/qrcode"
)

// Defaults.
const (
	DefaultURL     = "https://dwellings.tv/tv/"
	DefaultCaption = "Looking for something more specific?\nCustomize your channel through the QR code below"
	DefaultSeconds = 12
	DefaultColor   = "0x101418"

	// renderVersion is mixed into the hash: bump it when the layout or the
	// encode changes so an old bumper is not mistaken for the new one.
	renderVersion = "b1"
	qrSize        = 560 // QR pixels; on a qrCard white card
	qrCard        = 600
)

// Spec is everything the render depends on.
type Spec struct {
	URL             string // what the QR encodes
	Caption         string // lines separated by "\n"
	Seconds         int
	Background      string // ffmpeg colour, used without BackgroundImage
	BackgroundImage string // optional 16:9 image path
	Music           string // optional audio path; silence without
	FontPath        string
	FPS             int
}

// Hash identifies the render: the inputs, the bytes of the files among
// them, and renderVersion.
func (s Spec) Hash() (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%s|%s|%d|", renderVersion, s.URL, s.Caption, s.Seconds, s.Background, s.FontPath, s.FPS)
	for _, p := range []string{s.BackgroundImage, s.Music} {
		if p == "" {
			io.WriteString(h, "-|")
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
		io.WriteString(h, "|")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Render writes the bumper MP4 into workDir and returns its path.
func Render(ctx context.Context, s Spec, workDir string) (string, error) {
	qrPath := filepath.Join(workDir, "qr.png")
	if err := qrcode.WritePNG(s.URL, qrPath, qrSize); err != nil {
		return "", err
	}
	var captionFiles []string
	for i, line := range strings.Split(s.Caption, "\n") {
		p := filepath.Join(workDir, fmt.Sprintf("caption-%d.txt", i))
		if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
			return "", err
		}
		captionFiles = append(captionFiles, p)
	}
	urlFile := filepath.Join(workDir, "url.txt")
	shown := strings.TrimPrefix(strings.TrimPrefix(s.URL, "https://"), "http://")
	if err := os.WriteFile(urlFile, []byte(shown), 0o644); err != nil {
		return "", err
	}
	out := filepath.Join(workDir, "bumper.mp4")
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", buildFFmpegArgs(s, qrPath, captionFiles, urlFile, out)...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg bumper: %w: %s", err, tail(stderr.String(), 600))
	}
	return out, nil
}

// buildFFmpegArgs is the ffmpeg command line: pure, so tests can assert it.
// Inputs: 0 = background (image or colour), 1 = QR, 2 = audio (music or
// silence). Video: H.264 1080p yuv420p at s.FPS, keyframes every 3 s so
// every HLS segment is 3 s (well under hls.TargetDuration); audio AAC 160k.
func buildFFmpegArgs(s Spec, qrPath string, captionFiles []string, urlFile, outPath string) []string {
	secs := strconv.Itoa(s.Seconds)
	fps := strconv.Itoa(s.FPS)
	args := []string{"-y"}
	if s.BackgroundImage != "" {
		args = append(args, "-loop", "1", "-t", secs, "-i", s.BackgroundImage)
	} else {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=%s:s=1920x1080:r=%s:d=%s", s.Background, fps, secs))
	}
	args = append(args, "-i", qrPath)
	if s.Music != "" {
		args = append(args, "-stream_loop", "-1", "-i", s.Music)
	} else {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo")
	}

	var g []string
	g = append(g, fmt.Sprintf("[0:v]scale=1920:1080:force_original_aspect_ratio=increase,crop=1920:1080,setsar=1,fps=%s,format=yuv420p[bg]", fps))
	pad := (qrCard - qrSize) / 2
	g = append(g, fmt.Sprintf("[1:v]scale=%d:%d,pad=%d:%d:%d:%d:color=white[qr]", qrSize, qrSize, qrCard, qrCard, pad, pad))
	// The card sits a little below centre; the caption lines go above it,
	// the URL under it.
	g = append(g, "[bg][qr]overlay=(W-w)/2:(H-h)/2+70[v0]")
	var text strings.Builder
	text.WriteString("[v0]")
	for i, f := range captionFiles {
		fmt.Fprintf(&text, "drawtext=fontfile='%s':textfile='%s':fontsize=58:fontcolor=white:x=(w-tw)/2:y=%d,", s.FontPath, f, 96+i*76)
	}
	fmt.Fprintf(&text, "drawtext=fontfile='%s':textfile='%s':fontsize=40:fontcolor=0xDDDDDD:x=(w-tw)/2:y=h-110,", s.FontPath, urlFile)
	fmt.Fprintf(&text, "fade=t=in:st=0:d=0.5,fade=t=out:st=%s:d=0.5[outv]", trimFloat(float64(s.Seconds)-0.5))
	g = append(g, text.String())
	args = append(args, "-filter_complex", strings.Join(g, ";"), "-map", "[outv]", "-map", "2:a")

	if s.Music != "" {
		args = append(args, "-af", fmt.Sprintf("volume=0.5,afade=t=in:d=1,afade=t=out:st=%d:d=1", s.Seconds-1), "-shortest")
	}
	args = append(args,
		"-t", secs,
		"-c:a", "aac", "-b:a", "160k",
		"-r", fps,
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "veryfast", "-crf", "20",
		"-g", strconv.Itoa(3*s.FPS), "-keyint_min", strconv.Itoa(3*s.FPS), "-sc_threshold", "0",
		"-movflags", "+faststart",
		outPath,
	)
	return args
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
