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

// insert is a clip of our own (the ad-break slate, the QR bumper) that the
// lineup builder airs between listings every interval of air time. An
// interval of 0 leaves it out.
type insert struct {
	clip     ClipRef
	interval time.Duration
}

// buildItems picks a version's air order: a seeded shuffle of clips (sorted
// by id first so the input order is irrelevant), cut to maxMS. A lineup always
// has at least one clip, even if that clip alone exceeds maxMS. Each insert
// with a positive interval is then placed in front of every clip that would
// otherwise start interval or more after that insert last ended (for the
// first one: after the version's start); inserts due at the same boundary
// air in the order given. An insert is never the first item and never the
// last: a version starts and ends with content.
func buildItems(clips []ClipRef, inserts []insert, seed int64, maxMS int) (ids []int64, ms []int, segs []int) {
	order := make([]ClipRef, len(clips))
	copy(order, clips)
	sort.Slice(order, func(i, j int) bool { return order[i].ID < order[j].ID })
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	total := 0
	var items []ClipRef
	for _, c := range order {
		if len(items) > 0 && total+c.TotalMS > maxMS {
			break
		}
		items = append(items, c)
		total += c.TotalMS
	}
	items = withInserts(items, inserts)
	for _, c := range items {
		ids = append(ids, c.ID)
		ms = append(ms, c.TotalMS)
		segs = append(segs, c.Segments)
	}
	return ids, ms, segs
}

// withInserts places each insert with a positive interval before every clip
// of items that would start that interval or more of air time after the
// insert last ended (initially: after the start). Air time counts everything
// aired, other inserts included. The first item is always a clip, so is the
// last.
func withInserts(items []ClipRef, inserts []insert) []ClipRef {
	var active []insert
	for _, in := range inserts {
		if in.interval > 0 {
			active = append(active, in)
		}
	}
	if len(active) == 0 {
		return items
	}
	out := make([]ClipRef, 0, len(items)+len(items)/2)
	since := make([]int, len(active)) // ms aired since each insert last ended
	air := func(c ClipRef) {
		out = append(out, c)
		for k := range since {
			since[k] += c.TotalMS
		}
	}
	for i, c := range items {
		if i > 0 {
			for k, in := range active {
				if since[k] >= int(in.interval.Milliseconds()) {
					air(in.clip)
					since[k] = 0
				}
			}
		}
		air(c)
	}
	return out
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

// lineupSource is a channel's effective scope and eligible clips, resolved at
// most once and then reused by every version built from it. Resolving costs
// one ListClips scan of the library per fallback level plus a CityOfZip, and
// a single cold EPG request can materialise dozens of versions, so the
// resolution is shared across all of them rather than repeated per version.
// One source is created per request; it is not safe for concurrent use, and
// does not need to be (concurrent misses on a key share one leader through
// singleflight).
type lineupSource struct {
	svc      *Service
	key      string
	scope    Scope
	clips    []ClipRef
	bumper   *ClipRef // the current QR bumper as a lineup item; nil = none
	slate    *ClipRef // the current ad-break slate as a lineup item; nil = none
	resolved bool
}

// inserts lists what the builder airs between listings, in the order they
// take a shared boundary: the ad break first, then the QR bumper, so the
// invitation follows the ads straight into content (nil entries are
// skipped).
func (src *lineupSource) inserts(o Options) []insert {
	var ins []insert
	if src.slate != nil {
		ins = append(ins, insert{*src.slate, o.AdBreakInterval})
	}
	if src.bumper != nil {
		ins = append(ins, insert{*src.bumper, o.BumperInterval})
	}
	return ins
}

// currentInsert loads the newest bumper of kind as a lineup item when its
// interval is positive; nil when the interval is 0 or there is no row.
func (src *lineupSource) currentInsert(ctx context.Context, kind string, interval time.Duration) (*ClipRef, error) {
	if interval <= 0 {
		return nil, nil
	}
	b, err := src.svc.store.CurrentBumper(ctx, kind)
	if err != nil || b == nil {
		return nil, err
	}
	r := b.ref()
	return &r, nil
}

// newSource creates an unresolved lineupSource for key. Nothing is queried
// until a version actually has to be built.
func (s *Service) newSource(key string) *lineupSource { return &lineupSource{svc: s, key: key} }

func (src *lineupSource) resolve(ctx context.Context) error {
	if src.resolved {
		return nil
	}
	sc, err := ParseKey(src.key)
	if err != nil {
		return err
	}
	eff, clips, err := src.svc.resolveScope(ctx, sc)
	if err != nil {
		return err
	}
	if src.bumper, err = src.currentInsert(ctx, KindBumper, src.svc.opts.BumperInterval); err != nil {
		return err
	}
	if src.slate, err = src.currentInsert(ctx, KindSlate, src.svc.opts.AdBreakInterval); err != nil {
		return err
	}
	src.scope, src.clips, src.resolved = eff, clips, true
	return nil
}

// newVersion materialises version n of key starting at startsAt, continuing
// the counters of prev (nil for version 1), from src's resolved scope.
func (s *Service) newVersion(ctx context.Context, key string, n int, startsAt time.Time, prev *Version, src *lineupSource) (*Version, error) {
	if err := src.resolve(ctx); err != nil {
		return nil, err
	}
	ids, ms, segs := buildItems(src.clips, src.inserts(s.opts), seedFor(key, n), s.opts.LineupHours*3600*1000)
	v := &Version{Key: key, Version: n, Scope: src.scope.Key(), StartsAt: startsAt, ItemIDs: ids, ItemMS: ms, ItemSegs: segs}
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
func (s *Service) versionAt(ctx context.Context, key string, t time.Time, src *lineupSource) (cur, prev *Version, err error) {
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
		v, err := s.newVersion(ctx, key, n, startsAt, last, src)
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
func (s *Service) advanceChain(ctx context.Context, key string, horizon time.Time, budget int, src *lineupSource) (*Version, error) {
	now := s.now()
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
		v, err := s.newVersion(ctx, key, n, startsAt, last, src)
		if err != nil {
			return nil, err
		}
		ok, err := s.store.InsertVersion(ctx, v)
		if err != nil {
			return nil, err
		}
		if ok {
			s.log.Info("channel lineup version created", "channel", key, "version", n, "scope", v.Scope,
				"items", len(v.ItemIDs), "starts_at", v.StartsAt, "ends_at", v.EndsAt)
		}
	}
	// Report the chain's stored tip, never the version this call happened to
	// build: a concurrent instance may have won any of the inserts above, and
	// its row is the canonical one. This is also the answer when the budget
	// was exhausted (or zero) without creating anything.
	vs, err := s.store.LatestVersions(ctx, key, 1)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, nil
	}
	return &vs[0], nil
}
