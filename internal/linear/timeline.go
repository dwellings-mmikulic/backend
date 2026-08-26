package linear

import (
	"errors"
	"fmt"
	"time"

	"github.com/dwellingtw/backend/internal/hls"
)

// segment is one entry of a live playlist.
type segment struct {
	URL         string
	DurMS       int
	Start       time.Time
	Seq         int64 // media sequence number
	Item        int64 // global item index across the chain
	FirstOfItem bool
}

// itemAt returns the index of the item airing at t within v, or -1 when t is
// outside the version.
func itemAt(v *Version, t time.Time) int {
	off := t.Sub(v.StartsAt).Milliseconds()
	if off < 0 {
		return -1
	}
	var cum int64
	for i, ms := range v.ItemMS {
		cum += int64(ms)
		if off < cum {
			return i
		}
	}
	return -1
}

// windowSpec bounds a live playlist window. RFC 8216 §6.2.2 requires a live
// playlist to span at least three target durations, so the window is sized by
// duration, not by a segment count: segments are 3–8.33 s, and six of the
// short ones are only 18 s against a TARGETDURATION of 10.
type windowSpec struct {
	MinSegments int // never list fewer than this many ended segments
	MinMS       int // nor less than this much listed duration
}

// liveWindow is what every live playlist uses: 40 s (4 × TargetDuration,
// comfortably above the RFC's 3 ×) of ended segments, and never fewer than 6.
var liveWindow = windowSpec{MinSegments: 6, MinMS: 4 * hls.TargetDuration * 1000}

// satisfiedBy reports whether segs already meets both floors.
func (w windowSpec) satisfiedBy(segs []segment) bool {
	if len(segs) < w.MinSegments {
		return false
	}
	ms := 0
	for _, s := range segs {
		ms += s.DurMS
	}
	return ms >= w.MinMS
}

// trim keeps the newest segments that satisfy w, dropping everything older.
func (w windowSpec) trim(segs []segment) []segment {
	keep, ms := 0, 0
	for i := len(segs) - 1; i >= 0; i-- {
		keep++
		ms += segs[i].DurMS
		if keep >= w.MinSegments && ms >= w.MinMS {
			break
		}
	}
	return segs[len(segs)-keep:]
}

// backFrom returns the lowest item index i of v such that items [i, to) alone
// already carry w's floors, so a window ending inside item to can always be
// filled without expanding further back. Bounding by item count alone is not
// enough: an item is only guaranteed to hold one segment, and with 3 s
// segments a 40 s window can reach across many short items.
func backFrom(v *Version, to int, w windowSpec) int {
	if to <= 0 {
		return 0
	}
	if to > len(v.ItemMS) {
		to = len(v.ItemMS)
	}
	i := to
	ms, segs := 0, 0
	for i > 0 && (segs < w.MinSegments || ms < w.MinMS) {
		i--
		ms += v.ItemMS[i]
		segs += v.ItemSegs[i]
	}
	return i
}

// itemRange returns the item indexes [from, to] of v whose segments a window
// w ending at now could need: the item airing at now (or the last item if now
// is past the version) and enough items before it to fill w.
func itemRange(v *Version, now time.Time, w windowSpec) (from, to int) {
	to = itemAt(v, now)
	if to < 0 {
		to = len(v.ItemIDs) - 1
	}
	if to < 0 {
		return 0, -1 // an itemless version (only possible if written by hand)
	}
	return backFrom(v, to, w), to
}

// windowClipIDs lists the clip ids window may need, so the caller can fetch
// them in one query.
func windowClipIDs(cur, prev *Version, now time.Time, w windowSpec) []int64 {
	from, to := itemRange(cur, now, w)
	ids := append([]int64(nil), cur.ItemIDs[from:to+1]...)
	if prev != nil {
		ids = append(ids, prev.ItemIDs[backFrom(prev, len(prev.ItemIDs), w):]...)
	}
	return ids
}

// ErrInconsistentLineup reports a stored lineup that disagrees with the clips
// it references. It is never a client error: the handler answers 500 and logs
// the identifying detail.
var ErrInconsistentLineup = errors.New("inconsistent channel lineup")

// InconsistentLineupError identifies the item that disagrees.
type InconsistentLineupError struct {
	Channel string
	Version int
	Item    int   // index within the version
	ClipID  int64 // video_hls id the item references
	Want    int   // segments the version recorded in ItemSegs
	Got     int   // segments the clip actually has (0 when the clip is missing)
	Missing bool  // the clips lookup had no row for ClipID
}

func (e *InconsistentLineupError) Error() string {
	if e.Missing {
		return fmt.Sprintf("%s: clip %d (item %d of %s/%d) is missing, version says %d segments",
			ErrInconsistentLineup, e.ClipID, e.Item, e.Channel, e.Version, e.Want)
	}
	return fmt.Sprintf("%s: clip %d (item %d of %s/%d) has %d segments, version says %d",
		ErrInconsistentLineup, e.ClipID, e.Item, e.Channel, e.Version, e.Got, e.Want)
}

// Is makes errors.Is(err, ErrInconsistentLineup) work.
func (e *InconsistentLineupError) Is(target error) bool { return target == ErrInconsistentLineup }

// expandItems yields the segments of items [from, to] of v. Every item's
// media sequence number is derived solely from v.ItemSegs, the counter the
// chain's monotonic MEDIA-SEQUENCE contract (store.go) is built on — never
// from the clip's actual segment count — so a bad clips entry can never
// desynchronize the sequence numbers of items that follow it. If clips is
// missing an item's id, or the clip's segment count disagrees with what the
// version recorded in ItemSegs, that is a store inconsistency serious enough
// to corrupt every later segment's sequence number, so expandItems fails with
// an *InconsistentLineupError rather than emitting a lineup that silently
// violates the contract.
func expandItems(v *Version, clips map[int64]ClipSegments, from, to int) ([]segment, error) {
	var out []segment
	var startMS int64
	seq := v.StartSeq
	for i := 0; i < from; i++ {
		startMS += int64(v.ItemMS[i])
		seq += int64(v.ItemSegs[i])
	}
	for i := from; i <= to && i < len(v.ItemIDs); i++ {
		id := v.ItemIDs[i]
		c, ok := clips[id]
		if !ok || len(c.SegmentMS) != v.ItemSegs[i] {
			return nil, &InconsistentLineupError{
				Channel: v.Key, Version: v.Version, Item: i, ClipID: id,
				Want: v.ItemSegs[i], Got: len(c.SegmentMS), Missing: !ok,
			}
		}
		t := v.StartsAt.Add(time.Duration(startMS) * time.Millisecond)
		itemSeq := seq
		for j, ms := range c.SegmentMS {
			out = append(out, segment{
				URL:         c.BaseURL + "/" + hls.SegmentName(j),
				DurMS:       ms,
				Start:       t,
				Seq:         itemSeq + int64(j),
				Item:        v.StartItem + int64(i),
				FirstOfItem: j == 0,
			})
			t = t.Add(time.Duration(ms) * time.Millisecond)
		}
		seq += int64(v.ItemSegs[i])
		startMS += int64(v.ItemMS[i])
	}
	return out, nil
}

// ended keeps the segments that have finished by now.
func ended(segs []segment, now time.Time) []segment {
	out := segs[:0:0]
	for _, s := range segs {
		if !s.Start.Add(time.Duration(s.DurMS) * time.Millisecond).After(now) {
			out = append(out, s)
		}
	}
	return out
}

// covers reports whether v is the version airing at t.
func covers(v *Version, t time.Time) bool {
	return v != nil && !t.Before(v.StartsAt) && t.Before(v.EndsAt)
}

// window returns the newest segments that have ended by now and satisfy w,
// from cur and — when cur has too few yet — the tail of prev.
func window(cur, prev *Version, clips map[int64]ClipSegments, now time.Time, w windowSpec) ([]segment, error) {
	from, to := itemRange(cur, now, w)
	all, err := expandItems(cur, clips, from, to)
	if err != nil {
		return nil, err
	}
	segs := ended(all, now)
	if !w.satisfiedBy(segs) && prev != nil {
		pall, err := expandItems(prev, clips, backFrom(prev, len(prev.ItemIDs), w), len(prev.ItemIDs)-1)
		if err != nil {
			return nil, err
		}
		segs = append(ended(pall, now), segs...)
	}
	return w.trim(segs), nil
}
