package linear

import (
	"fmt"
	"io"

	"github.com/dwellingtw/backend/internal/hls"
)

// writePlaylist renders the live media playlist for the window segs.
//
// segs is legitimately empty at channel cold start (before any segment has
// ended, prev == nil) and whenever the clips lookup comes back empty; that
// case writes only the header lines, with MEDIA-SEQUENCE and
// DISCONTINUITY-SEQUENCE both 0 and no EXTINF entries, rather than panicking.
// A caller that would rather answer an empty window with an HTTP error (e.g.
// 503 while the channel warms up) must check len(segs) itself before
// calling.
//
// Every item boundary is an EXT-X-DISCONTINUITY (timestamps restart per
// clip). EXT-X-DISCONTINUITY-SEQUENCE counts the tags that belong to segments
// already gone from the window: with g the global item index of the first
// listed segment, that is g−1 tags for items 1..g−1, plus item g's own tag
// when the window starts mid-item.
//
// An ad-break slate (segment.inBreak) is cued the way SSAI stitchers read
// legacy SCTE-35 markers (AWS MediaPackage "SCTE-35 enhanced", MediaTailor,
// Wurl, Amagi): EXT-X-CUE-OUT:<seconds> on its first segment,
// EXT-X-CUE-OUT-CONT:ElapsedTime=…,Duration=… on every later one (and in
// place of CUE-OUT when the window opens mid-break, so a joining stitcher
// knows where in the break it is), EXT-X-CUE-IN on the first content segment
// after it. Each tag precedes the EXTINF it applies to, after the
// discontinuity and before PROGRAM-DATE-TIME. A break whose last segment has
// left the window takes its CUE-IN with it, as any tag goes with its segment.
func writePlaylist(w io.Writer, segs []segment) {
	var first segment
	var removed int64
	if len(segs) > 0 {
		first = segs[0]
		if first.Item > 0 {
			removed = first.Item - 1
			if !first.FirstOfItem {
				removed++
			}
		}
	}
	fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:%d\n#EXT-X-INDEPENDENT-SEGMENTS\n", hls.TargetDuration)
	fmt.Fprintf(w, "#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", first.Seq, removed)
	for i, s := range segs {
		disc := s.FirstOfItem && s.Item > 0
		if disc {
			io.WriteString(w, "#EXT-X-DISCONTINUITY\n")
		}
		switch {
		case s.inBreak() && i == 0 && s.ElapsedMS > 0:
			fmt.Fprintf(w, "#EXT-X-CUE-OUT-CONT:ElapsedTime=%s,Duration=%s\n", seconds(s.ElapsedMS), seconds(s.BreakMS))
		case s.inBreak() && s.FirstOfItem:
			fmt.Fprintf(w, "#EXT-X-CUE-OUT:%s\n", seconds(s.BreakMS))
		case s.inBreak():
			fmt.Fprintf(w, "#EXT-X-CUE-OUT-CONT:ElapsedTime=%s,Duration=%s\n", seconds(s.ElapsedMS), seconds(s.BreakMS))
		case i > 0 && segs[i-1].inBreak():
			io.WriteString(w, "#EXT-X-CUE-IN\n")
		}
		if i == 0 || disc {
			fmt.Fprintf(w, "#EXT-X-PROGRAM-DATE-TIME:%s\n", s.Start.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		fmt.Fprintf(w, "#EXTINF:%.3f,\n%s\n", float64(s.DurMS)/1000, s.URL)
	}
}

// seconds formats a millisecond count as the playlist's "%.3f" seconds.
func seconds(ms int) string { return fmt.Sprintf("%.3f", float64(ms)/1000) }

// writeMaster renders the master playlist. All clips share one rendition, so
// there is a single variant; the attributes describe the listing encode
// (H.264 High 4.0 1080p30, AAC-LC).
//
// EXT-X-VERSION is 6: INDEPENDENT-SEGMENTS, DISCONTINUITY-SEQUENCE,
// AVERAGE-BANDWIDTH and FRAME-RATE are all past version 3.
func writeMaster(w io.Writer, mediaURI string) {
	io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	io.WriteString(w, "#EXT-X-STREAM-INF:BANDWIDTH=1400000,AVERAGE-BANDWIDTH=1100000,CODECS=\"avc1.640028,mp4a.40.2\",RESOLUTION=1920x1080,FRAME-RATE=30.000\n")
	fmt.Fprintf(w, "%s\n", mediaURI)
}
