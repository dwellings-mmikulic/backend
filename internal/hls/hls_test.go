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
