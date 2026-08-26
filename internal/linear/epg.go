package linear

import (
	"context"
	"strconv"
	"time"
)

// programBlock is the EPG granularity.
const programBlock = 30 * time.Minute

// epgChainBudget bounds how many new lineup versions a single EPG request
// may materialise (see advanceChain). Kept well under maxChain so a thin
// scope's request degrades to partial horizon coverage instead of an error,
// and so a cache-missing GET never triggers more than a handful of
// synchronous lineup builds + version writes.
const epgChainBudget = 8

// EPG is the schedule of one channel.
type EPG struct {
	Channel  Channel   `json:"channel"`
	Programs []Program `json:"programs"`
}

// Channel identifies a channel and the scope it actually airs.
type Channel struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
	Name  string `json:"name"`
}

// Program is one EPG block.
type Program struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Listings    int       `json:"listings"`
}

// EPG returns programBlock-sized programs covering
// [now − 1 block, now + EPGHorizonHours), or as much of that as the chain
// currently reaches, advancing the chain
// by up to epgChainBudget versions to extend that coverage. A channel with
// little content per version may not reach the full horizon in one call —
// the window still starts at now and grows on later polls as the persisted
// chain is extended further (see advanceChain) — so the response is always
// bounded regardless of how far the horizon sits or how long the channel has
// existed, instead of the request materialising the entire history-to-date
// or every version needed to reach the horizon synchronously.
func (s *Service) EPG(ctx context.Context, sc Scope, now time.Time) (EPG, error) {
	key := sc.Key()
	horizon := now.Add(time.Duration(s.opts.EPGHorizonHours) * time.Hour)
	// First make sure the chain reaches now at all — the same guarantee
	// Playlist relies on (versionAt, bounded by maxChain), so even a thin
	// scope that never catches up to the horizon in one call still has a
	// "now" to build a window from. Only the further reach toward horizon,
	// which is what can run unboundedly long for a thin scope, is subject to
	// epgChainBudget below.
	if _, _, err := s.current(ctx, key, now); err != nil {
		return EPG{}, err
	}
	latest, err := s.advanceChain(ctx, key, horizon, epgChainBudget)
	if err != nil {
		return EPG{}, err
	}
	if latest == nil {
		return EPG{}, ErrNoContent
	}
	// upper is how far the chain actually reaches this call: horizon itself
	// once a later poll has caught the chain up to it, latest.EndsAt while
	// it hasn't.
	upper := horizon
	if latest.EndsAt.Before(upper) {
		upper = latest.EndsAt
	}
	// The window starts one block before now, not at the channel's very first
	// version: a channel running for weeks can carry hundreds of past
	// versions, and walking all of them into the response (and into the
	// ListingsByClipID query below) would make both grow without bound purely
	// with channel age. The block already airing is included so a guide can
	// show what is on now.
	start := now.Truncate(programBlock).Add(-programBlock)

	// Only the versions overlapping [start, upper) are loaded, so neither the
	// query, the items nor the listings lookup scale with the channel's full
	// history — only with the window actually being reported.
	versions, err := s.store.ListVersionsBetween(ctx, key, start, upper)
	if err != nil {
		return EPG{}, err
	}
	if len(versions) == 0 {
		return EPG{}, ErrNoContent
	}

	// Every item start within [start, upper), with the scope it airs under.
	type aired struct {
		id    int64
		start time.Time
		scope string
	}
	var items []aired
	var ids []int64
	for _, v := range versions {
		t := v.StartsAt
		for i, id := range v.ItemIDs {
			if !t.Before(start) && t.Before(upper) {
				items = append(items, aired{id: id, start: t, scope: v.Scope})
				ids = append(ids, id)
			}
			t = t.Add(time.Duration(v.ItemMS[i]) * time.Millisecond)
		}
	}
	listings, err := s.store.ListingsByClipID(ctx, ids)
	if err != nil {
		return EPG{}, err
	}
	price := make(map[int64]int64, len(listings))
	for _, l := range listings {
		price[l.ClipID] = l.Price
	}

	effScope, _ := ParseKey(latest.Scope)
	e := EPG{Channel: Channel{Key: key, Scope: latest.Scope, Name: effScope.Name()}}
	i := 0
	for b := start; b.Before(upper); b = b.Add(programBlock) {
		end := b.Add(programBlock)
		var n int
		var lo, hi int64
		scope := ""
		for ; i < len(items) && items[i].start.Before(end); i++ {
			if items[i].start.Before(b) {
				continue
			}
			n++
			scope = items[i].scope
			if p := price[items[i].id]; p > 0 {
				if lo == 0 || p < lo {
					lo = p
				}
				if p > hi {
					hi = p
				}
			}
		}
		if n == 0 {
			continue // idle gap between versions: nothing aired
		}
		blockScope, _ := ParseKey(scope)
		e.Programs = append(e.Programs, Program{
			Start: b, End: end, Title: blockScope.Name(), Description: describe(n, lo, hi), Listings: n,
		})
	}
	return e, nil
}

// describe formats "12 listings · $285,000–$1,150,000". Zero prices are omitted.
func describe(n int, lo, hi int64) string {
	s := strconv.Itoa(n) + " listing"
	if n != 1 {
		s += "s"
	}
	switch {
	case lo > 0 && hi > lo:
		s += " · $" + humanInt(lo) + "–$" + humanInt(hi)
	case lo > 0:
		s += " · $" + humanInt(lo)
	}
	return s
}

func humanInt(n int64) string {
	d := strconv.FormatInt(n, 10)
	var out []byte
	for i := 0; i < len(d); i++ {
		if i > 0 && (len(d)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d[i])
	}
	return string(out)
}
