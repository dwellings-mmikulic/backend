package linear

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

// ErrUnknownHousehold reports a household id no feed was ever created for.
var ErrUnknownHousehold = errors.New("unknown household")

// citiesTTL is how long Cities reuses its answer: the list changes only as
// the library grows, and the mobile page fetches it on every open.
const citiesTTL = 10 * time.Minute

// Household returns h's spans, oldest first.
func (s *Service) Household(ctx context.Context, h viewer.ID) ([]Span, error) {
	spans, err := s.store.Spans(ctx, h)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, ErrUnknownHousehold
	}
	return spans, nil
}

// EnsureHousehold returns h's spans, creating span 0 at now when h is new.
// The default channel is the first candidate (the IP's ZIP, city, state —
// see geoCandidates) that resolves to something more specific than
// national, else national. Span 0 has zero offsets: a fresh feed's counters
// are its channel's own.
func (s *Service) EnsureHousehold(ctx context.Context, h viewer.ID, candidates []Scope, now time.Time) ([]Span, error) {
	spans, err := s.store.Spans(ctx, h)
	if err != nil {
		return nil, err
	}
	if len(spans) > 0 {
		return spans, nil
	}
	sp := Span{Household: h, N: 0, Scope: Scope{}.Key(), Source: SourceDefault, StartsAt: now, CreatedAt: now}
	for _, c := range candidates {
		eff, _, err := s.resolveScope(ctx, c)
		if err != nil {
			if errors.Is(err, ErrNoContent) {
				continue
			}
			return nil, err
		}
		if eff != (Scope{}) {
			sp.Scope, sp.Requested, sp.Source = eff.Key(), c.Key(), SourceGeo
			break
		}
	}
	if _, err := s.store.InsertSpan(ctx, &sp); err != nil {
		return nil, err
	}
	// Re-read: a concurrent first request may have won the insert, and its
	// row is the one every later request will see.
	return s.Household(ctx, h)
}

// Cities lists the areas with enough content for a channel of their own.
func (s *Service) Cities(ctx context.Context) ([]City, error) {
	s.citiesMu.Lock()
	defer s.citiesMu.Unlock()
	if s.cities != nil && s.now().Sub(s.citiesAt) < citiesTTL {
		return s.cities, nil
	}
	cs, err := s.store.ListCities(ctx, s.opts.MinScopeClips)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		cs = []City{}
	}
	s.cities, s.citiesAt = cs, s.now()
	return cs, nil
}

// lookback bounds how far back a span can still contribute to a window: a
// window is at most 6 segments of ≤ 10 s plus one more, so anything that
// ended more than two minutes ago is out of it.
const lookback = 2 * time.Minute

// spanFrom is when sp's channel segments start to count: its start, or an
// hour earlier for span 0 so a fresh feed has history to serve at once,
// exactly like a fresh channel (historyLead).
func spanFrom(sp Span) time.Time {
	if sp.N == 0 {
		return sp.StartsAt.Add(-historyLead)
	}
	return sp.StartsAt
}

// shifted relabels channel segments as sp's personal ones: the offsets
// applied, and after a switch the first segment marked as an item start so
// writePlaylist emits the discontinuity there even mid-clip.
func shifted(segs []segment, sp Span) []segment {
	out := make([]segment, len(segs))
	for i, g := range segs {
		g.Seq += sp.SeqOffset
		g.Item += sp.ItemOffset
		out[i] = g
	}
	if sp.N > 0 && len(out) > 0 {
		out[0].FirstOfItem = true
	}
	return out
}

// contribution is what sp adds to its feed up to end: the segments of its
// channel that start at or after spanFrom(sp) and have ended by end,
// relabelled. end is the span's own end (the next span's start) or now.
func (s *Service) contribution(ctx context.Context, sp Span, end time.Time) ([]segment, error) {
	segs, err := s.channelWindow(ctx, sp.Scope, end)
	if err != nil {
		return nil, err
	}
	from := spanFrom(sp)
	i := 0
	for i < len(segs) && segs[i].Start.Before(from) {
		i++
	}
	return shifted(segs[i:], sp), nil
}

// PersonalPlaylist renders household h's live playlist at now: the spans'
// contributions, oldest first, trimmed to the usual window. airing is the
// span on air at now.
func (s *Service) PersonalPlaylist(ctx context.Context, h viewer.ID, now time.Time) ([]byte, Span, error) {
	spans, err := s.Household(ctx, h)
	if err != nil {
		return nil, Span{}, err
	}
	airing := spans[0]
	var segs []segment
	for k, sp := range spans {
		if sp.StartsAt.After(now) {
			break // not airing yet
		}
		airing = sp
		end := now
		if k+1 < len(spans) && spans[k+1].StartsAt.Before(end) {
			end = spans[k+1].StartsAt
		}
		if end.Before(now.Add(-lookback)) {
			continue
		}
		c, err := s.contribution(ctx, sp, end)
		if err != nil {
			return nil, Span{}, err
		}
		segs = append(segs, c...)
	}
	if len(segs) == 0 {
		return nil, Span{}, ErrNoContent
	}
	var buf bytes.Buffer
	writePlaylist(&buf, liveWindow.trim(segs))
	return buf.Bytes(), airing, nil
}

// endOfAiring is when the segment of channel key airing at now ends — the
// earliest moment a switch away from key takes effect — or now when nothing
// is airing.
func (s *Service) endOfAiring(ctx context.Context, key string, now time.Time) (time.Time, error) {
	cur, _, err := s.current(ctx, key, now, s.newSource(key))
	if err != nil {
		return time.Time{}, err
	}
	i := itemAt(cur, now)
	if i < 0 {
		return now, nil
	}
	clips, err := s.store.ClipsByID(ctx, cur.ItemIDs[i:i+1])
	if err != nil {
		return time.Time{}, err
	}
	segs, err := expandItems(cur, clips, i, i)
	if err != nil {
		return time.Time{}, err
	}
	for _, g := range segs {
		end := g.Start.Add(time.Duration(g.DurMS) * time.Millisecond)
		if !g.Start.After(now) && end.After(now) {
			return end, nil
		}
	}
	return now, nil
}

// firstAtOrAfter is the first segment of channel key starting at or after t:
// in the version covering t, or — when t falls inside that version's last
// segment — the next version, materialised here if it does not exist yet.
func (s *Service) firstAtOrAfter(ctx context.Context, key string, t time.Time) (segment, error) {
	v, _, err := s.current(ctx, key, t, s.newSource(key))
	if err != nil {
		return segment{}, err
	}
	for {
		i := itemAt(v, t)
		if i < 0 {
			i = 0 // t precedes v (v is the next version): its first segment
		}
		to := i + 1
		if to > len(v.ItemIDs)-1 {
			to = len(v.ItemIDs) - 1
		}
		clips, err := s.store.ClipsByID(ctx, v.ItemIDs[i:to+1])
		if err != nil {
			return segment{}, err
		}
		segs, err := expandItems(v, clips, i, to)
		if err != nil {
			return segment{}, err
		}
		for _, g := range segs {
			if !g.Start.Before(t) {
				return g, nil
			}
		}
		next, _, err := s.current(ctx, key, v.EndsAt, s.newSource(key))
		if err != nil {
			return segment{}, err
		}
		if next.Version == v.Version {
			return segment{}, fmt.Errorf("channel %s: no segment at or after %s", key, t)
		}
		v = next
	}
}

// splice builds span n of a feed: channel eff from S on, chained after prev
// so the personal counters continue from prev's last segment.
func (s *Service) splice(ctx context.Context, prev Span, eff, requested Scope, S time.Time, n int, now time.Time) (*Span, error) {
	tail, err := s.contribution(ctx, prev, S)
	if err != nil {
		return nil, err
	}
	first, err := s.firstAtOrAfter(ctx, eff.Key(), S)
	if err != nil {
		return nil, err
	}
	sp := &Span{Household: prev.Household, N: n, Scope: eff.Key(), Requested: requested.Key(), Source: SourceChoice, StartsAt: S, CreatedAt: now}
	if len(tail) > 0 {
		last := tail[len(tail)-1]
		sp.SeqOffset = last.Seq + 1 - first.Seq
		sp.ItemOffset = last.Item + 1 - first.Item
	}
	return sp, nil
}

// Choose points household h's feed at the channel for requested (after the
// usual thin-scope fallback) from the next segment boundary on, and returns
// the household's spans. Choosing the channel already on air is a no-op. A
// choice made before the previous one aired a single segment replaces it
// rather than chaining after it: there is nothing of it to splice after.
func (s *Service) Choose(ctx context.Context, h viewer.ID, requested Scope, now time.Time) ([]Span, error) {
	if err := s.checkArea(ctx, requested); err != nil {
		return nil, err
	}
	eff, _, err := s.resolveScope(ctx, requested)
	if err != nil {
		return nil, err
	}
	spans, err := s.Household(ctx, h)
	if err != nil {
		return nil, err
	}
	latest := spans[len(spans)-1]
	if latest.Scope == eff.Key() {
		return spans, nil
	}
	aired, err := s.contribution(ctx, latest, now)
	if err != nil {
		return nil, err
	}
	prev, n := latest, latest.N+1
	var S time.Time
	if len(aired) == 0 && latest.N > 0 {
		prev, n, S = spans[len(spans)-2], latest.N, latest.StartsAt
	} else if S, err = s.endOfAiring(ctx, latest.Scope, now); err != nil {
		return nil, err
	}
	sp, err := s.splice(ctx, prev, eff, requested, S, n, now)
	if err != nil {
		return nil, err
	}
	sp.Household = h
	if n == latest.N {
		err = s.store.ReplaceSpan(ctx, sp)
	} else {
		_, err = s.store.InsertSpan(ctx, sp) // a lost race means the other submit's span wins
	}
	if err != nil {
		return nil, err
	}
	s.log.Info("household feed switched", "span", sp.N, "scope", sp.Scope, "requested", sp.Requested, "starts_at", sp.StartsAt)
	return s.Household(ctx, h)
}
