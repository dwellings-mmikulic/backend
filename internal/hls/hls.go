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
	"time"
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
	// A killed ffmpeg must not leave Run blocked on its stderr pipe: that would hold a work slot past the claim's lease.
	cmd.WaitDelay = 5 * time.Second
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
