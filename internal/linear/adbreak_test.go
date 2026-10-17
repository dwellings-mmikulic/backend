package linear

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"
)

// Break fixture (times relative to t0): clip A (3 s, 3 s, 2 s), then the
// slate S (5 s, 5 s: a 10 s ad break), then clip C (5 s, 5 s) → [t0, t0+28 s).
func breakFixture() (v *Version, clips map[int64]ClipSegments) {
	clips = map[int64]ClipSegments{
		1:             {ID: 1, BaseURL: "https://cdn/hls/v1/A/x", SegmentMS: []int{3000, 3000, 2000}},
		bumperItem(5): {ID: bumperItem(5), BaseURL: "https://cdn/hls/slates/S/x", SegmentMS: []int{5000, 5000}, Break: true},
		3:             {ID: 3, BaseURL: "https://cdn/hls/v1/C/x", SegmentMS: []int{5000, 5000}},
	}
	v = &Version{Key: "us", Version: 1, StartsAt: t0, EndsAt: t0.Add(28 * time.Second),
		ItemIDs: []int64{1, bumperItem(5), 3}, ItemMS: []int{8000, 10000, 10000}, ItemSegs: []int{3, 2, 2}}
	return v, clips
}

func TestWritePlaylist_CueOutAndInAroundASlate(t *testing.T) {
	v, clips := breakFixture()
	segs := mustWindow(t, v, nil, clips, t0.Add(28*time.Second), windowSpec{MinSegments: 10})
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
#EXT-X-CUE-OUT:10.000
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:08.000Z
#EXTINF:5.000,
https://cdn/hls/slates/S/x/seg-000.ts
#EXT-X-CUE-OUT-CONT:ElapsedTime=5.000,Duration=10.000
#EXTINF:5.000,
https://cdn/hls/slates/S/x/seg-001.ts
#EXT-X-DISCONTINUITY
#EXT-X-CUE-IN
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:18.000Z
#EXTINF:5.000,
https://cdn/hls/v1/C/x/seg-000.ts
#EXTINF:5.000,
https://cdn/hls/v1/C/x/seg-001.ts
`
	if buf.String() != want {
		t.Errorf("playlist:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// A window that opens inside the break (the slate's first segment has
// already left it) tells a joining stitcher where in the break it is with
// CUE-OUT-CONT in place of CUE-OUT, and still closes it with CUE-IN.
func TestWritePlaylist_WindowOpeningMidBreak(t *testing.T) {
	v, clips := breakFixture()
	segs := mustWindow(t, v, nil, clips, t0.Add(28*time.Second), windowSpec{MinSegments: 3})
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	want := `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:10
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA-SEQUENCE:4
#EXT-X-DISCONTINUITY-SEQUENCE:1
#EXT-X-CUE-OUT-CONT:ElapsedTime=5.000,Duration=10.000
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:13.000Z
#EXTINF:5.000,
https://cdn/hls/slates/S/x/seg-001.ts
#EXT-X-DISCONTINUITY
#EXT-X-CUE-IN
#EXT-X-PROGRAM-DATE-TIME:2026-08-25T12:00:18.000Z
#EXTINF:5.000,
https://cdn/hls/v1/C/x/seg-000.ts
#EXTINF:5.000,
https://cdn/hls/v1/C/x/seg-001.ts
`
	if buf.String() != want {
		t.Errorf("playlist:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// Six two-minute clips with a one-minute slate every four minutes of air
// time and a 12 s bumper on the same interval: at a boundary where both are
// due the slate airs first, then the bumper, then content. The slate is
// never first or last, and the clips keep their shuffled order.
func TestBuildItems_SlateThenBumperWhenBothDue(t *testing.T) {
	var clips []ClipRef
	for id := int64(1); id <= 6; id++ {
		clips = append(clips, ClipRef{ID: id, TotalMS: 120000, Segments: 40})
	}
	slate := ClipRef{ID: bumperItem(9), TotalMS: 60000, Segments: 20}
	bumper := ClipRef{ID: bumperItem(3), TotalMS: 12000, Segments: 4}
	max := 24 * 3600 * 1000
	plain, _, _ := buildItems(clips, nil, seedFor("us", 1), max)
	ids, ms, segs := buildItems(clips, []insert{{slate, 4 * time.Minute}, {bumper, 4 * time.Minute}}, seedFor("us", 1), max)
	want := []int64{plain[0], plain[1], slate.ID, bumper.ID, plain[2], plain[3], slate.ID, bumper.ID, plain[4], plain[5]}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if ms[2] != 60000 || segs[2] != 20 || ms[3] != 12000 || segs[3] != 4 {
		t.Errorf("slate/bumper ms,segs = %d,%d / %d,%d", ms[2], segs[2], ms[3], segs[3])
	}
	// Slate alone, bumper off (interval 0): the bumper never appears.
	ids, _, _ = buildItems(clips, []insert{{slate, 4 * time.Minute}, {bumper, 0}}, seedFor("us", 1), max)
	if !reflect.DeepEqual(ids, []int64{plain[0], plain[1], slate.ID, plain[2], plain[3], slate.ID, plain[4], plain[5]}) {
		t.Errorf("slate only: %v", ids)
	}
}

// The store keeps slates and QR bumpers apart by kind: "current" is per
// kind, and a slate resolves as a break while a bumper does not.
func TestMemStore_SlatesAreBreaks(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	m.addBumper(1, 3000, 3000, 3000, 3000)
	m.addSlate(2, 5000, 5000)
	m.addBumper(3, 4000, 4000, 4000)
	if b, err := m.CurrentBumper(ctx, KindSlate); err != nil || b == nil || b.ID != 2 || b.Kind != KindSlate {
		t.Fatalf("current slate = %+v err=%v (want id 2)", b, err)
	}
	if b, err := m.CurrentBumper(ctx, KindBumper); err != nil || b == nil || b.ID != 3 {
		t.Fatalf("current bumper = %+v err=%v (want id 3, the slate is not a bumper)", b, err)
	}
	clips, err := m.ClipsByID(ctx, []int64{bumperItem(2), bumperItem(3)})
	if err != nil {
		t.Fatal(err)
	}
	if !clips[bumperItem(2)].Break || clips[bumperItem(3)].Break {
		t.Errorf("break flags: slate=%v bumper=%v", clips[bumperItem(2)].Break, clips[bumperItem(3)].Break)
	}
}

// A channel with both a slate and a bumper on file airs the slate every
// AdBreakInterval and, at a boundary where both are due, the slate first.
func TestVersion_AirsSlateBeforeBumper(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 200) // 200 one-minute clips
	m.addBumper(1, 3000, 3000, 3000, 3000)
	m.addSlate(2, 5000, 5000, 5000, 5000, 5000, 5000)
	s := testService(m, t0)
	s.opts.BumperInterval = 5 * time.Minute
	s.opts.AdBreakInterval = 10 * time.Minute

	cur, _, err := s.versionAt(context.Background(), "zip:77494", t0, s.newSource("zip:77494"))
	if err != nil {
		t.Fatal(err)
	}
	var slates, bumpers int
	for i, id := range cur.ItemIDs {
		switch id {
		case bumperItem(2):
			slates++
			if cur.ItemMS[i] != 30000 || cur.ItemSegs[i] != 6 {
				t.Errorf("slate item %d: ms=%d segs=%d", i, cur.ItemMS[i], cur.ItemSegs[i])
			}
			if i+1 < len(cur.ItemIDs) && cur.ItemIDs[i+1] != bumperItem(1) {
				t.Errorf("item %d after the slate is %d, want the bumper (both due every 10 min)", i+1, cur.ItemIDs[i+1])
			}
		case bumperItem(1):
			bumpers++
		}
	}
	// 200 min of content: slates before the clips starting at 10, 20, … min;
	// bumpers at 5, 10, 15, … so twice as many, and never two slates in a row.
	if slates < 15 || bumpers < 2*slates-1 {
		t.Errorf("slates=%d bumpers=%d in %v", slates, bumpers, cur.ItemIDs)
	}
	// Interval 0: no slate at all.
	m2 := newMemStore()
	addClips(m2, katy, 1, 200)
	m2.addSlate(2, 5000, 5000)
	s2 := testService(m2, t0)
	cur, _, err = s2.versionAt(context.Background(), "zip:77494", t0, s2.newSource("zip:77494"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cur.ItemIDs {
		if isBumper(id) {
			t.Fatalf("slate aired with AdBreakInterval 0: %v", cur.ItemIDs)
		}
	}
}

// A window that opens after the break carries no cue at all: the CUE-IN
// left with the segment it was attached to, as any other tag does.
func TestWritePlaylist_NoCueAfterTheBreakLeftTheWindow(t *testing.T) {
	v, clips := breakFixture()
	segs := mustWindow(t, v, nil, clips, t0.Add(28*time.Second), windowSpec{MinSegments: 2})
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	if bytes.Contains(buf.Bytes(), []byte("CUE")) {
		t.Errorf("playlist carries a cue with no slate segment listed:\n%s", buf.String())
	}
}
