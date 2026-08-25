package linear

import (
	"context"
	"strconv"
	"time"
)

// programBlock is the EPG granularity.
const programBlock = 30 * time.Minute

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

// EPG returns programBlock-sized programs from the channel's first version
// through now + EPGHorizonHours, materialising versions as far as needed.
func (s *Service) EPG(ctx context.Context, sc Scope, now time.Time) (EPG, error) {
	key := sc.Key()
	horizon := now.Add(time.Duration(s.opts.EPGHorizonHours) * time.Hour)
	latest, _, err := s.versionAt(ctx, key, horizon)
	if err != nil {
		return EPG{}, err
	}
	versions, err := s.store.ListVersions(ctx, key)
	if err != nil {
		return EPG{}, err
	}

	// Every item start within the range, with the scope it airs under.
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
			if !t.After(horizon) {
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
	for b := versions[0].StartsAt.Truncate(programBlock); b.Before(horizon); b = b.Add(programBlock) {
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
