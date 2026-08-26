package linear

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Two-version fixture (times relative to t0):
//
//	v1: item A (3 s, 3 s, 2 s) then item B (4 s, 4 s)  → [t0, t0+16 s)
//	v2: item C (5 s, 5 s)                                → [t0+16 s, t0+26 s)
func fixture() (v1, v2 *Version, clips map[int64]ClipSegments) {
	clips = map[int64]ClipSegments{
		1: {ID: 1, BaseURL: "https://cdn/hls/v1/A/x", SegmentMS: []int{3000, 3000, 2000}},
		2: {ID: 2, BaseURL: "https://cdn/hls/v1/B/x", SegmentMS: []int{4000, 4000}},
		3: {ID: 3, BaseURL: "https://cdn/hls/v1/C/x", SegmentMS: []int{5000, 5000}},
	}
	v1 = &Version{Key: "us", Version: 1, StartsAt: t0, EndsAt: t0.Add(16 * time.Second),
		StartSeq: 0, StartItem: 0, ItemIDs: []int64{1, 2}, ItemMS: []int{8000, 8000}, ItemSegs: []int{3, 2}}
	v2 = &Version{Key: "us", Version: 2, StartsAt: t0.Add(16 * time.Second), EndsAt: t0.Add(26 * time.Second),
		StartSeq: 5, StartItem: 2, ItemIDs: []int64{3}, ItemMS: []int{10000}, ItemSegs: []int{2}}
	return v1, v2, clips
}

func TestWindow_ListsOnlyEndedSegments(t *testing.T) {
	v1, _, clips := fixture()
	segs := mustWindow(t, v1, nil, clips, t0.Add(12*time.Second), windowSpec{MinSegments: 6})
	// Ended by t0+12: A0 [0,3) A1 [3,6) A2 [6,8) B0 [8,12). B1 ends at 16.
	if len(segs) != 4 {
		t.Fatalf("got %d segments, want 4", len(segs))
	}
	wantURLs := []string{
		"https://cdn/hls/v1/A/x/seg-000.ts", "https://cdn/hls/v1/A/x/seg-001.ts",
		"https://cdn/hls/v1/A/x/seg-002.ts", "https://cdn/hls/v1/B/x/seg-000.ts",
	}
	for i, s := range segs {
		if s.URL != wantURLs[i] {
			t.Errorf("seg %d url = %s, want %s", i, s.URL, wantURLs[i])
		}
		if s.Seq != int64(i) {
			t.Errorf("seg %d seq = %d", i, s.Seq)
		}
	}
	if !segs[3].FirstOfItem || segs[3].Item != 1 || segs[2].FirstOfItem {
		t.Errorf("item boundaries wrong: %+v", segs)
	}
	if !segs[3].Start.Equal(t0.Add(8 * time.Second)) {
		t.Errorf("B0 start = %s, want t0+8s", segs[3].Start)
	}
}

func TestWritePlaylist_Golden(t *testing.T) {
	v1, _, clips := fixture()
	segs := mustWindow(t, v1, nil, clips, t0.Add(12*time.Second), windowSpec{MinSegments: 6})
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	want := `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:10
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-DISCONTINUITY-SEQUENCE:0
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:00.000Z
#EXTINF:3.000,
https://cdn/hls/v1/A/x/seg-000.ts
#EXTINF:3.000,
https://cdn/hls/v1/A/x/seg-001.ts
#EXTINF:2.000,
https://cdn/hls/v1/A/x/seg-002.ts
#EXT-X-DISCONTINUITY
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:08.000Z
#EXTINF:4.000,
https://cdn/hls/v1/B/x/seg-000.ts
`
	if buf.String() != want {
		t.Errorf("playlist:\n%s\nwant:\n%s", buf.String(), want)
	}
	if strings.Contains(buf.String(), "ENDLIST") {
		t.Error("live playlist must not contain EXT-X-ENDLIST")
	}
}

func TestWindow_SpansVersionBoundaryAndCountsRemovedDiscontinuities(t *testing.T) {
	v1, v2, clips := fixture()

	// t0+21: C0 [16,21) has ended. Window of 3 → B0 B1 C0.
	segs := mustWindow(t, v2, v1, clips, t0.Add(21*time.Second), windowSpec{MinSegments: 3})
	if len(segs) != 3 || segs[0].URL != "https://cdn/hls/v1/B/x/seg-000.ts" || segs[2].URL != "https://cdn/hls/v1/C/x/seg-000.ts" {
		t.Fatalf("window = %+v", segs)
	}
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	out := buf.String()
	// First segment is B0: seq 3, item 1 and its first segment → the only
	// discontinuity tag ever removed is none (B's own tag is still listed).
	if !strings.Contains(out, "#EXT-X-MEDIA-SEQUENCE:3\n") || !strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE:0\n") {
		t.Errorf("counters wrong:\n%s", out)
	}
	if strings.Count(out, "#EXT-X-DISCONTINUITY\n") != 2 { // before B0 and before C0
		t.Errorf("want 2 discontinuity tags:\n%s", out)
	}

	// t0+26: C1 has ended. Window of 3 → B1 C0 C1: B's tag was removed.
	segs = mustWindow(t, v2, v1, clips, t0.Add(26*time.Second), windowSpec{MinSegments: 3})
	buf.Reset()
	writePlaylist(&buf, segs)
	out = buf.String()
	if !strings.Contains(out, "#EXT-X-MEDIA-SEQUENCE:4\n") || !strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n") {
		t.Errorf("counters wrong:\n%s", out)
	}
}

func TestWindowClipIDs_CoversWindowAndPrevTail(t *testing.T) {
	v1, v2, clips := fixture()
	ids := windowClipIDs(v2, v1, t0.Add(17*time.Second), windowSpec{MinSegments: 6})
	for _, want := range []int64{1, 2, 3} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("ids %v missing %d", ids, want)
		}
	}
	_ = clips
}

var seqRe = regexp.MustCompile(`#EXT-X-MEDIA-SEQUENCE:(\d+)\n#EXT-X-DISCONTINUITY-SEQUENCE:(\d+)\n`)

func TestPlaylist_CountersNeverDecrease(t *testing.T) {
	v1, v2, clips := fixture()
	lastSeq, lastDisc := int64(-1), int64(-1)
	for now := t0.Add(3 * time.Second); now.Before(v2.EndsAt); now = now.Add(time.Second) {
		cur, prev := v1, (*Version)(nil)
		if !now.Before(v2.StartsAt) {
			cur, prev = v2, v1
		}
		segs := mustWindow(t, cur, prev, clips, now, windowSpec{MinSegments: 3})
		var buf bytes.Buffer
		writePlaylist(&buf, segs)
		m := seqRe.FindStringSubmatch(buf.String())
		if m == nil {
			t.Fatalf("no counters at %s:\n%s", now, buf.String())
		}
		seq, _ := strconv.ParseInt(m[1], 10, 64)
		disc, _ := strconv.ParseInt(m[2], 10, 64)
		if seq < lastSeq || disc < lastDisc {
			t.Errorf("at %s counters went backwards: seq %d→%d disc %d→%d", now, lastSeq, seq, lastDisc, disc)
		}
		lastSeq, lastDisc = seq, disc
	}
}

// window legitimately returns empty at channel cold start: no segment has
// ended yet (prev == nil, now still inside the first item).
func TestWindow_ColdStartIsEmpty(t *testing.T) {
	v1, _, clips := fixture()
	segs := mustWindow(t, v1, nil, clips, t0, windowSpec{MinSegments: 6})
	if len(segs) != 0 {
		t.Fatalf("got %d segments at cold start, want 0", len(segs))
	}
}

// writePlaylist must not panic on an empty window; it should still emit
// valid header lines with both counters at 0 and no EXTINF entries.
func TestWritePlaylist_EmptyWindow(t *testing.T) {
	var buf bytes.Buffer
	writePlaylist(&buf, nil)
	out := buf.String()
	want := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:10\n#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-DISCONTINUITY-SEQUENCE:0\n"
	if out != want {
		t.Errorf("empty window playlist:\n%s\nwant:\n%s", out, want)
	}
	if strings.Contains(out, "EXTINF") {
		t.Error("empty window playlist should have no segments")
	}
}

func TestWriteMaster(t *testing.T) {
	var buf bytes.Buffer
	writeMaster(&buf, "live.m3u8?zip=77494")
	want := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1400000,AVERAGE-BANDWIDTH=1100000,CODECS=\"avc1.640028,mp4a.40.2\",RESOLUTION=1920x1080,FRAME-RATE=30.000\n" +
		"live.m3u8?zip=77494\n"
	if buf.String() != want {
		t.Errorf("master:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// mustWindow is window() with the store-inconsistency error turned into a
// test failure.
func mustWindow(t *testing.T, cur, prev *Version, clips map[int64]ClipSegments, now time.Time, w windowSpec) []segment {
	t.Helper()
	segs, err := window(cur, prev, clips, now, w)
	if err != nil {
		t.Fatalf("window at %s: %v", now, err)
	}
	return segs
}
