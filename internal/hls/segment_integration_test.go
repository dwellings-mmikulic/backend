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
