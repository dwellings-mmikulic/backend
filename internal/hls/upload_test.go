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
