package music

import (
	"bytes"
	"errors"
	"testing"

	"github.com/dwellingtw/backend/internal/music/musictest"
)

const clipPTS = 132000 // first picture of every clip

func openLoop(t *testing.T, frames int) *Loop {
	t.Helper()
	l, err := Open(musictest.WriteLoop(t, frames))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

type audioFrame struct {
	pts  int64
	data []byte
}

// split separates a segment into its audio frames, the continuity counters
// of its audio packets and everything that is not audio.
func split(t *testing.T, seg []byte) (frames []audioFrame, ccs []int, rest []byte) {
	t.Helper()
	if len(seg)%tsPacket != 0 {
		t.Fatalf("segment is %d bytes, not whole packets", len(seg))
	}
	for off := 0; off < len(seg); off += tsPacket {
		p := seg[off : off+tsPacket]
		if pidOf(p) != musictest.AudioPID {
			rest = append(rest, p...)
			continue
		}
		ccs = append(ccs, int(p[3]&0x0F))
		b := payload(p)
		if p[1]&0x40 != 0 {
			pts, ok := pesPTS(b)
			if !ok {
				t.Fatalf("audio PES at %d has no timestamp", off)
			}
			frames = append(frames, audioFrame{pts: pts})
			b = b[pesHeader:]
		}
		last := &frames[len(frames)-1]
		last.data = append(last.data, b...)
	}
	return frames, ccs, rest
}

func TestOpen_IndexesFrames(t *testing.T) {
	l := openLoop(t, 50)
	if l.Frames() != 50 || l.SampleRate() != musictest.SampleRate {
		t.Fatalf("frames %d, sample rate %d", l.Frames(), l.SampleRate())
	}
	for _, i := range []int64{0, 49, 50, 149} {
		f, err := l.frame(i)
		if err != nil || !bytes.Equal(f, musictest.Frame(int(i%50))) {
			t.Errorf("frame %d: wrong bytes (err %v)", i, err)
		}
	}
}

func TestRemux_ReplacesTheSoundAndKeepsThePicture(t *testing.T) {
	l := openLoop(t, 1000)
	video := musictest.Segment(clipPTS, 60, true) // 2 s, the first of its clip
	const start = 1_000_000

	out, err := l.Remux(video, start, 2000, 0)
	if err != nil {
		t.Fatal(err)
	}
	frames, _, rest := split(t, out)
	_, _, wantRest := split(t, video)
	if !bytes.Equal(rest, wantRest) {
		t.Error("packets other than audio changed")
	}
	first, end := l.frameAt(start), l.frameAt(start+2000)
	if len(frames) != int(end-first) {
		t.Fatalf("%d audio frames, want %d", len(frames), end-first)
	}
	for k, f := range frames {
		i := first + int64(k)
		if !bytes.Equal(f.data, musictest.Frame(int(i%1000))) {
			t.Fatalf("audio frame %d is not loop frame %d", k, i)
		}
		if want := clipPTS + l.frameTicks(i) - start*90; f.pts != want {
			t.Fatalf("audio frame %d has timestamp %d, want %d", k, f.pts, want)
		}
	}
	// The first frame starts within one frame before the first picture.
	if d := clipPTS - frames[0].pts; d < 0 || d > 2090 {
		t.Errorf("first frame is %d ticks before the first picture", d)
	}
}

func TestRemux_FramesArriveAheadOfTheClock(t *testing.T) {
	l := openLoop(t, 1000)
	out, err := l.Remux(musictest.Segment(clipPTS, 90, true), 5000, 3000, 0)
	if err != nil {
		t.Fatal(err)
	}
	var clock int64 = -1
	early := 0
	for off := 0; off < len(out); off += tsPacket {
		p := out[off : off+tsPacket]
		if pcr, ok := pcrOf(p); ok {
			clock = pcr
		}
		if pidOf(p) != musictest.AudioPID || p[1]&0x40 == 0 || clock < 0 {
			continue
		}
		pts, _ := pesPTS(payload(p))
		if pts < clock {
			t.Fatalf("frame with timestamp %d placed after clock %d", pts, clock)
		}
		// The pictures run 0.7 s ahead of the clock, and the frames after
		// the last clock reference go out with the last picture.
		if pts-clock > 63000+3000+2090 {
			early++
		}
	}
	if early > 0 {
		t.Errorf("%d frames placed further ahead of the clock than the pictures", early)
	}
}

func TestRemux_MusicAndCountersContinueAcrossSegments(t *testing.T) {
	l := openLoop(t, 400) // 9.3 s, so the loop wraps inside the test
	const clipStart = 7_000
	durs := []int{2000, 3000, 2000, 5000}
	var frames []audioFrame
	var ccs []int
	elapsed := 0
	for _, d := range durs {
		video := musictest.Segment(clipPTS+int64(elapsed)*90, d*30/1000, true)
		out, err := l.Remux(video, clipStart+int64(elapsed), d, elapsed)
		if err != nil {
			t.Fatal(err)
		}
		f, c, _ := split(t, out)
		frames, ccs = append(frames, f...), append(ccs, c...)
		elapsed += d
	}
	first := l.frameAt(clipStart)
	if want := int(l.frameAt(clipStart+int64(elapsed)) - first); len(frames) != want {
		t.Fatalf("%d frames over the clip, want %d", len(frames), want)
	}
	for k, f := range frames {
		i := first + int64(k)
		if !bytes.Equal(f.data, musictest.Frame(int(i%400))) {
			t.Fatalf("frame %d is not loop frame %d", k, i%400)
		}
		if k > 0 {
			if d := f.pts - frames[k-1].pts; d != 2089 && d != 2090 {
				t.Fatalf("timestamp step %d at frame %d", d, k)
			}
		}
	}
	for k := 1; k < len(ccs); k++ {
		if ccs[k] != (ccs[k-1]+1)&0x0F {
			t.Fatalf("continuity counter jumps from %d to %d at audio packet %d", ccs[k-1], ccs[k], k)
		}
	}
	if ccs[0] != 0 {
		t.Errorf("a clip's first audio packet has counter %d, want 0", ccs[0])
	}
}

func TestRemux_NextClipCarriesOnInTheLoop(t *testing.T) {
	l := openLoop(t, 1000)
	a, err := l.Remux(musictest.Segment(clipPTS+4000*90, 60, true), 20_000, 2000, 4000) // a clip's last segment
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Remux(musictest.Segment(clipPTS, 60, true), 22_000, 2000, 0) // the next clip's first
	if err != nil {
		t.Fatal(err)
	}
	fa, _, _ := split(t, a)
	fb, _, _ := split(t, b)
	last, next := l.frameAt(22_000)-1, l.frameAt(22_000)
	if !bytes.Equal(fa[len(fa)-1].data, musictest.Frame(int(last))) || !bytes.Equal(fb[0].data, musictest.Frame(int(next))) {
		t.Error("the loop does not carry on from one clip into the next")
	}
}

func TestRemux_WorksAtTheLimits(t *testing.T) {
	l := openLoop(t, 1000)
	for name, a := range map[string][2]int64{
		"the year 2046":              {2_400_000_000_000, 0},
		"latest start, deep in clip": {MaxStartMS, MaxElapsedMS},
	} {
		video := musictest.Segment(clipPTS+a[1]*90, 60, true)
		out, err := l.Remux(video, a[0], 2000, int(a[1]))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		frames, _, _ := split(t, out)
		if want := int(l.frameAt(a[0]+2000) - l.frameAt(a[0])); len(frames) != want {
			t.Fatalf("%s: %d audio frames, want %d", name, len(frames), want)
		}
		// The first frame starts within one frame before the first picture.
		if d := clipPTS + a[1]*90 - frames[0].pts; d < 0 || d > 2090 {
			t.Errorf("%s: first frame is %d ticks before the first picture", name, d)
		}
		for k := 1; k < len(frames); k++ {
			if d := frames[k].pts - frames[k-1].pts; d != 2089 && d != 2090 {
				t.Fatalf("%s: timestamp step %d at frame %d", name, d, k)
			}
		}
	}
}

func TestFrameTicks_MatchesThePlainProductWhereItFits(t *testing.T) {
	l := openLoop(t, 10)
	for _, i := range []int64{0, 1, 44099, 44100, 44101, 77_000_000_000, 100_000_000_000} {
		if got, want := l.frameTicks(i), i*samplesPerFrame*90000/musictest.SampleRate; got != want {
			t.Errorf("frameTicks(%d) = %d, want %d", i, got, want)
		}
	}
}

func TestRemux_RefusesWhatItCannotSwap(t *testing.T) {
	l := openLoop(t, 100)
	good := musictest.Segment(clipPTS, 30, true)
	cases := map[string][]byte{
		"no sound track":       musictest.Segment(clipPTS, 30, false),
		"not whole packets":    good[:len(good)-1],
		"empty":                nil,
		"lost sync mid-way":    append(append([]byte{}, good[:tsPacket*5]...), make([]byte, tsPacket)...),
		"tables without video": good[:tsPacket*2],
	}
	for name, seg := range cases {
		if _, err := l.Remux(seg, 1000, 1000, 0); !errors.Is(err, ErrNotRemuxable) {
			t.Errorf("%s: err = %v, want ErrNotRemuxable", name, err)
		}
	}
	for name, a := range map[string][3]int64{
		"no duration":          {1000, 0, 0},
		"duration over limit":  {1000, MaxSegmentMS + 1, 0},
		"elapsed over limit":   {200_000_000_000_000, 1000, 200_000_000_000_000},
		"start over limit":     {MaxStartMS + 1, 1000, 0},
		"start overflows":      {9_000_000_000_000_000_000, 1000, 0},
		"negative elapsed":     {1000, 1000, -1},
		"elapsed before epoch": {1000, 1000, 2000},
	} {
		if _, err := l.Remux(good, a[0], int(a[1]), int(a[2])); err == nil || errors.Is(err, ErrNotRemuxable) {
			t.Errorf("%s: err = %v, want a timing error", name, err)
		}
	}
}
