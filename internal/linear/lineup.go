package linear

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

// ErrNoContent means the scope, after fallback, has no current clips.
var ErrNoContent = errors.New("no content for channel")

const (
	// historyLead is how far in the past a fresh chain starts, so the very
	// first request can already serve a full window.
	historyLead = time.Hour
	// idleGrace: a channel not requested this long past its latest version's
	// end restarts historyLead before now instead of chaining through dead
	// time nobody watched.
	idleGrace = 5 * time.Minute
	// maxChain bounds how many versions one request may create.
	maxChain = 64
)

// seedFor derives the shuffle seed of a version. Any instance computing the
// same (key, version) over the same clip set gets the same order.
func seedFor(key string, version int) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key + "|" + strconv.Itoa(version)))
	return int64(h.Sum64())
}

// buildItems picks a version's air order: a seeded shuffle of clips (sorted
// by id first so the input order is irrelevant), cut to maxMS. A lineup always
// has at least one clip, even if that clip alone exceeds maxMS.
func buildItems(clips []ClipRef, seed int64, maxMS int) (ids []int64, ms []int, segs []int) {
	order := make([]ClipRef, len(clips))
	copy(order, clips)
	sort.Slice(order, func(i, j int) bool { return order[i].ID < order[j].ID })
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	total := 0
	for _, c := range order {
		if len(ids) > 0 && total+c.TotalMS > maxMS {
			break
		}
		ids = append(ids, c.ID)
		ms = append(ms, c.TotalMS)
		segs = append(segs, c.Segments)
		total += c.TotalMS
	}
	return ids, ms, segs
}

// resolveScope walks zip → city → state → national until a scope has at
// least MinScopeClips current clips, returning the effective scope and its
// clips. The national scope is used whatever its size; if even it is empty
// the result is ErrNoContent.
func (s *Service) resolveScope(ctx context.Context, sc Scope) (Scope, []ClipRef, error) {
	for {
		clips, err := s.store.ListClips(ctx, sc)
		if err != nil {
			return Scope{}, nil, err
		}
		if len(clips) >= s.opts.MinScopeClips || sc == (Scope{}) {
			if len(clips) == 0 {
				return Scope{}, nil, ErrNoContent
			}
			return sc, clips, nil
		}
		switch {
		case sc.Zip != "":
			city, state, err := s.store.CityOfZip(ctx, sc.Zip)
			if err != nil {
				return Scope{}, nil, err
			}
			if city != "" && state != "" {
				sc = Scope{City: city, State: state}
			} else {
				sc = Scope{}
			}
		case sc.City != "":
			sc = Scope{State: sc.State}
		default:
			sc = Scope{}
		}
	}
}

// newVersion materialises version n of key starting at startsAt, continuing
// the counters of prev (nil for version 1).
func (s *Service) newVersion(ctx context.Context, key string, n int, startsAt time.Time, prev *Version) (*Version, error) {
	sc, err := ParseKey(key)
	if err != nil {
		return nil, err
	}
	eff, clips, err := s.resolveScope(ctx, sc)
	if err != nil {
		return nil, err
	}
	ids, ms, segs := buildItems(clips, seedFor(key, n), s.opts.LineupHours*3600*1000)
	v := &Version{Key: key, Version: n, Scope: eff.Key(), StartsAt: startsAt, ItemIDs: ids, ItemMS: ms, ItemSegs: segs}
	v.EndsAt = startsAt.Add(time.Duration(v.TotalMS()) * time.Millisecond)
	if prev != nil {
		v.StartSeq = prev.StartSeq + prev.Segments()
		v.StartItem = prev.StartItem + int64(len(prev.ItemIDs))
	}
	return v, nil
}

// versionAt returns the version covering t and its predecessor (nil for
// version 1), creating versions as needed. Creation races between instances
// are settled by the primary key: after every insert the chain is re-read, so
// the stored row — whoever wrote it — is the one served.
func (s *Service) versionAt(ctx context.Context, key string, t time.Time) (cur, prev *Version, err error) {
	now := s.now()
	for i := 0; i < maxChain; i++ {
		vs, err := s.store.LatestVersions(ctx, key, 2)
		if err != nil {
			return nil, nil, err
		}
		var (
			n        int
			startsAt time.Time
			last     *Version
		)
		switch {
		case len(vs) == 0:
			n, startsAt = 1, now.Add(-historyLead).Truncate(time.Minute)
		case t.Before(vs[0].StartsAt):
			return nil, nil, fmt.Errorf("time %s precedes channel %s version %d start %s", t, key, vs[0].Version, vs[0].StartsAt)
		case t.Before(vs[0].EndsAt):
			cur = &vs[0]
			if len(vs) > 1 {
				prev = &vs[1]
			}
			return cur, prev, nil
		default:
			last = &vs[0]
			n = last.Version + 1
			startsAt = last.EndsAt
			if now.After(last.EndsAt.Add(idleGrace)) {
				startsAt = now.Add(-historyLead).Truncate(time.Minute)
				if startsAt.Before(last.EndsAt) {
					startsAt = last.EndsAt
				}
			}
		}
		v, err := s.newVersion(ctx, key, n, startsAt, last)
		if err != nil {
			return nil, nil, err
		}
		if _, err := s.store.InsertVersion(ctx, v); err != nil {
			return nil, nil, err
		}
		s.log.Info("channel lineup version created", "channel", key, "version", n, "scope", v.Scope,
			"items", len(v.ItemIDs), "starts_at", v.StartsAt, "ends_at", v.EndsAt)
	}
	return nil, nil, fmt.Errorf("channel %s: could not reach %s within %d versions", key, t, maxChain)
}
