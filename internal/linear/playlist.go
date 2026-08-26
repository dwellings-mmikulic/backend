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
		if i == 0 || disc {
			fmt.Fprintf(w, "#EXT-X-PROGRAM-DATE-TIME:%s\n", s.Start.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		fmt.Fprintf(w, "#EXTINF:%.3f,\n%s\n", float64(s.DurMS)/1000, s.URL)
	}
}

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
