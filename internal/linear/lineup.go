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
//
// "The version covering t" is looked up with VersionsAt, i.e. among the
// versions that have already started at t, never from the chain's tip: an EPG
// request extends the chain up to a day into the future (advanceChain), and
// deriving the current version from the tip would make every request for now
// on a channel whose chain reaches beyond now fail. A new version is created
// only when the whole chain ends at or before t, so extending it can never
// collide with a version number that already exists.
func (s *Service) versionAt(ctx context.Context, key string, t time.Time) (cur, prev *Version, err error) {
	now := s.now()
	for i := 0; i < maxChain; i++ {
		vs, err := s.store.VersionsAt(ctx, key, t)
		if err != nil {
			return nil, nil, err
		}
		serve := func() (*Version, *Version) {
			if len(vs) > 1 {
				return &vs[0], &vs[1]
			}
			return &vs[0], nil
		}
		if len(vs) > 0 && t.Before(vs[0].EndsAt) {
			cur, prev = serve()
			return cur, prev, nil
		}
		tips, err := s.store.LatestVersions(ctx, key, 1)
		if err != nil {
			return nil, nil, err
		}
		var (
			n        int
			startsAt time.Time
			last     *Version
		)
		switch {
		case len(tips) == 0:
			n, startsAt = 1, now.Add(-historyLead).Truncate(time.Minute)
		case len(vs) == 0:
			// Every stored version starts after t: t predates the channel.
			return nil, nil, fmt.Errorf("time %s precedes channel %s version %d start %s", t, key, tips[0].Version, tips[0].StartsAt)
		case tips[0].EndsAt.After(t):
			// t falls in dead air the chain jumped over (an idle restart
			// leaves a hole between the old tip's end and the restart). The
			// version numbers past t already exist, so nothing can be created
			// here; serve the version that ended most recently instead.
			cur, prev = serve()
			return cur, prev, nil
		default:
			last = &tips[0]
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

// advanceChain grows key's version chain toward horizon, materialising at
// most budget new versions in this call. Unlike versionAt it never errors
// merely for not reaching horizon: a scope near MinScopeClips produces short
// versions (buildItems never repeats clips, so a version lasts only as long
// as the scope's total clip minutes), and forcing a single request to chain
// all the way to a horizon many hours out — as EPG's horizon is — can need
// far more versions than is safe to build synchronously in one HTTP request,
// or even more than maxChain allows at all. advanceChain instead returns
// however far the chain reaches within its budget; because versions persist,
// the next call resumes the chain where this one left off, so the covered
// horizon grows across successive polls instead of the request failing or
// blocking on dozens of synchronous lineup builds. Returns nil only if the
// channel has no content and none could be created.
func (s *Service) advanceChain(ctx context.Context, key string, horizon time.Time, budget int) (*Version, error) {
	now := s.now()
	var tip *Version
	for i := 0; i < budget; i++ {
		vs, err := s.store.LatestVersions(ctx, key, 1)
		if err != nil {
			return nil, err
		}
		if len(vs) > 0 && horizon.Before(vs[0].EndsAt) {
			return &vs[0], nil // chain already reaches the horizon
		}
		var (
			n        int
			startsAt time.Time
			last     *Version
		)
		if len(vs) == 0 {
			n, startsAt = 1, now.Add(-historyLead).Truncate(time.Minute)
		} else {
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
			return nil, err
		}
		if _, err := s.store.InsertVersion(ctx, v); err != nil {
			return nil, err
		}
		s.log.Info("channel lineup version created", "channel", key, "version", n, "scope", v.Scope,
			"items", len(v.ItemIDs), "starts_at", v.StartsAt, "ends_at", v.EndsAt)
		tip = v
	}
	if tip != nil {
		return tip, nil
	}
	// budget was exhausted (or zero) without this call creating anything new
	// (e.g. the chain already reached horizon on the very first check above,
	// or budget <= 0): report whatever the chain's current tip is.
	vs, err := s.store.LatestVersions(ctx, key, 1)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, nil
	}
	return &vs[0], nil
}
