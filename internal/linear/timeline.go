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

// itemRange returns the item indexes [from, to] of v whose segments a window
// of n ending at now could need: the item airing at now (or the last item if
// now is past the version) and the n before it, since an item has at least
// one segment.
func itemRange(v *Version, now time.Time, n int) (from, to int) {
	to = itemAt(v, now)
	if to < 0 {
		to = len(v.ItemIDs) - 1
	}
	return max(to-n, 0), to
}

// windowClipIDs lists the clip ids window may need, so the caller can fetch
// them in one query.
func windowClipIDs(cur, prev *Version, now time.Time, n int) []int64 {
	from, to := itemRange(cur, now, n)
	ids := append([]int64(nil), cur.ItemIDs[from:to+1]...)
	if prev != nil {
		pfrom := max(len(prev.ItemIDs)-n, 0)
		ids = append(ids, prev.ItemIDs[pfrom:]...)
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

// window returns the last n segments that have ended by now, from cur and —
// when cur has too few yet — the tail of prev.
func window(cur, prev *Version, clips map[int64]ClipSegments, now time.Time, n int) ([]segment, error) {
	from, to := itemRange(cur, now, n)
	all, err := expandItems(cur, clips, from, to)
	if err != nil {
		return nil, err
	}
	segs := ended(all, now)
	if len(segs) < n && prev != nil {
		pall, err := expandItems(prev, clips, max(len(prev.ItemIDs)-n, 0), len(prev.ItemIDs)-1)
		if err != nil {
			return nil, err
		}
		segs = append(ended(pall, now), segs...)
	}
	if len(segs) > n {
		segs = segs[len(segs)-n:]
	}
	return segs, nil
}
