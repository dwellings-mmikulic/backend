package linear

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/hls"
)

// TestRepository_Integration exercises every Store method against a real
// PostgreSQL, which is the only place the SQL, the INTEGER[]/BIGINT[]
// round-trips and the ON CONFLICT behaviour can actually be checked — the
// in-memory store in the unit tests reimplements them rather than running
// them.
//
// Skipped unless TEST_DATABASE_URL points at a database it may write to:
//
//	TEST_DATABASE_URL=postgres://dwellings:dwellings@localhost:5432/dwellings?sslmode=disable \
//	    go test ./internal/linear/ -run TestRepository_Integration
//
// Every row it writes is namespaced by a per-run suffix and removed again, so
// it can run against a database that already holds data.
func TestRepository_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the linear repository integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered first so it runs last: t.Cleanup is LIFO, and the row
	// cleanup below still needs the pool.
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var (
		suffix   = strconv.FormatInt(time.Now().UnixNano(), 36)
		zpidCur  = "it-cur-" + suffix
		zpidOld  = "it-old-" + suffix
		testZip  = "it" + suffix[len(suffix)-3:]
		city     = "Integrationville " + suffix
		state    = "TX"
		key      = "it:" + suffix
		hashCur  = "hash-cur-" + suffix
		hashOld  = "hash-old-" + suffix
		hashGone = "hash-superseded-" + suffix
	)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		for _, q := range []string{
			`DELETE FROM video_hls WHERE zpid = ANY($1)`,
			`DELETE FROM properties WHERE zpid = ANY($1)`,
		} {
			if _, err := pool.Exec(cleanupCtx, q, []string{zpidCur, zpidOld}); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM channel_lineups WHERE channel_key = $1`, key); err != nil {
			t.Errorf("cleanup lineups: %v", err)
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM zip_codes WHERE zip = $1`, testZip); err != nil {
			t.Errorf("cleanup zip: %v", err)
		}
	})

	r := NewRepository(pool)
	scope := Scope{Zip: testZip}

	countBefore, err := r.CountCurrentClips(ctx)
	if err != nil {
		t.Fatalf("count current clips: %v", err)
	}

	insertProperty(ctx, t, pool, zpidCur, city, state, testZip, hashCur, 500000)
	// The stale listing's current render is hashOld, but the only segments
	// that exist for it are from a superseded render.
	insertProperty(ctx, t, pool, zpidOld, city, state, testZip, hashOld, 400000)
	if _, err := pool.Exec(ctx, `INSERT INTO zip_codes (zip, city, state) VALUES ($1, $2, $3)
		ON CONFLICT (zip) DO NOTHING`, testZip, city, state); err != nil {
		t.Fatalf("insert zip code: %v", err)
	}

	// --- SetVideoHLS: rows are immutable -------------------------------
	clip := hls.Clip{SegmentMS: []int{3000, 4000, 2500}, TotalMS: 9500}
	base := "https://cdn.example/hls/v1/" + zpidCur + "/abcdef12"
	if err := r.SetVideoHLS(ctx, zpidCur, hashCur, base, clip); err != nil {
		t.Fatalf("set video hls: %v", err)
	}
	if err := r.SetVideoHLS(ctx, zpidCur, hashCur, "https://cdn.example/wrong", hls.Clip{SegmentMS: []int{1}, TotalMS: 1}); err != nil {
		t.Fatalf("second set video hls: %v", err)
	}
	if err := r.SetVideoHLS(ctx, zpidOld, hashGone, "https://cdn.example/hls/v1/"+zpidOld+"/00000000",
		hls.Clip{SegmentMS: []int{5000}, TotalMS: 5000}); err != nil {
		t.Fatalf("set video hls for the superseded render: %v", err)
	}

	// --- ListClips: only the current render, at every scope -------------
	cur := requireOneClip(ctx, t, r, scope)
	if cur.TotalMS != clip.TotalMS || cur.Segments != len(clip.SegmentMS) {
		t.Errorf("clip = %+v, want total %d ms in %d segments (the second SetVideoHLS must be a no-op)",
			cur, clip.TotalMS, len(clip.SegmentMS))
	}
	requireOneClip(ctx, t, r, Scope{City: normalise(city), State: "tx"})
	for _, sc := range []Scope{{State: "tx"}, {}} {
		clips, err := r.ListClips(ctx, sc)
		if err != nil {
			t.Fatalf("list clips %s: %v", sc.Key(), err)
		}
		// A shared database holds other rows, so the wider scopes are only
		// checked for containment; the zip and city scopes above are exact
		// and already prove the superseded render is excluded.
		if !hasClip(clips, cur.ID) {
			t.Errorf("scope %s does not contain the current clip %d", sc.Key(), cur.ID)
		}
	}

	// --- CountCurrentClips ----------------------------------------------
	countAfter, err := r.CountCurrentClips(ctx)
	if err != nil {
		t.Fatalf("count current clips: %v", err)
	}
	if countAfter != countBefore+1 {
		t.Errorf("current clips went %d → %d, want exactly one more (the superseded render must not count)", countBefore, countAfter)
	}

	// --- CityOfZip -------------------------------------------------------
	gotCity, gotState, err := r.CityOfZip(ctx, testZip)
	if err != nil {
		t.Fatalf("city of zip: %v", err)
	}
	if gotCity != normalise(city) || gotState != "tx" {
		t.Errorf("CityOfZip = %q, %q, want %q, %q", gotCity, gotState, normalise(city), "tx")
	}
	if c, s, err := r.CityOfZip(ctx, "nosuchzip"); err != nil || c != "" || s != "" {
		t.Errorf("CityOfZip of an unknown zip = %q, %q, %v; want empty, empty, nil", c, s, err)
	}

	// --- AreaExists ------------------------------------------------------
	for _, tc := range []struct {
		scope Scope
		want  bool
	}{
		{Scope{Zip: testZip}, true},
		{Scope{Zip: "nosuchzip"}, false},
		{Scope{City: normalise(city), State: "tx"}, true},
		{Scope{City: "nowhere at all", State: "tx"}, false},
		{Scope{}, true},
	} {
		got, err := r.AreaExists(ctx, tc.scope)
		if err != nil {
			t.Fatalf("area exists %s: %v", tc.scope.Key(), err)
		}
		if got != tc.want {
			t.Errorf("AreaExists(%s) = %v, want %v", tc.scope.Key(), got, tc.want)
		}
	}

	// --- InsertVersion / VersionsAt / ListVersionsBetween -----------------
	now := time.Now().UTC().Truncate(time.Millisecond)
	v1 := &Version{
		Key: key, Version: 1, Scope: scope.Key(),
		StartsAt: now.Add(-2 * time.Hour), EndsAt: now.Add(-time.Hour),
		StartSeq: 0, StartItem: 0,
		ItemIDs: []int64{cur.ID}, ItemMS: []int{clip.TotalMS}, ItemSegs: []int{len(clip.SegmentMS)},
	}
	v2 := &Version{
		Key: key, Version: 2, Scope: scope.Key(),
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
		StartSeq: 3, StartItem: 1,
		ItemIDs: []int64{cur.ID, cur.ID}, ItemMS: []int{clip.TotalMS, clip.TotalMS}, ItemSegs: []int{3, 3},
	}
	for _, v := range []*Version{v1, v2} {
		ok, err := r.InsertVersion(ctx, v)
		if err != nil {
			t.Fatalf("insert version %d: %v", v.Version, err)
		}
		if !ok {
			t.Fatalf("insert version %d reported not stored", v.Version)
		}
	}
	if ok, err := r.InsertVersion(ctx, v1); err != nil || ok {
		t.Errorf("re-inserting version 1 = %v, %v; want false, nil (the primary key settles the race)", ok, err)
	}

	at, err := r.VersionsAt(ctx, key, now)
	if err != nil {
		t.Fatalf("versions at: %v", err)
	}
	if len(at) != 2 || at[0].Version != 2 || at[1].Version != 1 {
		t.Fatalf("VersionsAt(now) = %v, want versions [2 1]", versionNumbers(at))
	}
	assertVersionEqual(t, &at[0], v2)
	if before, err := r.VersionsAt(ctx, key, now.Add(-3*time.Hour)); err != nil || len(before) != 0 {
		t.Errorf("VersionsAt before version 1 = %v, %v; want none", versionNumbers(before), err)
	}
	if only, err := r.VersionsAt(ctx, key, now.Add(-90*time.Minute)); err != nil || len(only) != 1 || only[0].Version != 1 {
		t.Errorf("VersionsAt inside version 1 = %v, %v; want [1]", versionNumbers(only), err)
	}

	both, err := r.ListVersionsBetween(ctx, key, now.Add(-90*time.Minute), now)
	if err != nil {
		t.Fatalf("list versions between: %v", err)
	}
	if len(both) != 2 || both[0].Version != 1 || both[1].Version != 2 {
		t.Errorf("ListVersionsBetween overlapping both = %v, want [1 2] ascending", versionNumbers(both))
	}
	later, err := r.ListVersionsBetween(ctx, key, now, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("list versions between: %v", err)
	}
	if len(later) != 1 || later[0].Version != 2 {
		t.Errorf("ListVersionsBetween after version 1 ended = %v, want [2]", versionNumbers(later))
	}
	if tip, err := r.LatestVersions(ctx, key, 1); err != nil || len(tip) != 1 || tip[0].Version != 2 {
		t.Errorf("LatestVersions = %v, %v; want [2]", versionNumbers(tip), err)
	}

	// --- ClipsByID / ListingsByClipID ------------------------------------
	byID, err := r.ClipsByID(ctx, []int64{cur.ID})
	if err != nil {
		t.Fatalf("clips by id: %v", err)
	}
	got, ok := byID[cur.ID]
	if !ok {
		t.Fatalf("ClipsByID did not return clip %d", cur.ID)
	}
	if got.BaseURL != base {
		t.Errorf("base url = %q, want %q (immutable)", got.BaseURL, base)
	}
	if len(got.SegmentMS) != len(clip.SegmentMS) {
		t.Fatalf("segment_ms = %v, want %v", got.SegmentMS, clip.SegmentMS)
	}
	for i, ms := range clip.SegmentMS {
		if got.SegmentMS[i] != ms {
			t.Errorf("segment %d = %d ms, want %d", i, got.SegmentMS[i], ms)
		}
	}

	listings, err := r.ListingsByClipID(ctx, []int64{cur.ID})
	if err != nil {
		t.Fatalf("listings by clip id: %v", err)
	}
	if len(listings) != 1 {
		t.Fatalf("listings = %+v, want 1", listings)
	}
	if l := listings[0]; l.ClipID != cur.ID || l.ZPID != zpidCur || l.Price != 500000 || l.City != city || l.State != state {
		t.Errorf("listing = %+v", l)
	}
}

func insertProperty(ctx context.Context, t *testing.T, pool *pgxpool.Pool, zpid, city, state, zip, hash string, price int64) {
	t.Helper()
	const q = `
INSERT INTO properties (zpid, address, city, state, zip, sale_price, video_status, video_url, video_content_hash)
VALUES ($1, $2, $3, $4, $5, $6, 'ready', $7, $8)`
	_, err := pool.Exec(ctx, q, zpid, "1 Test Way", city, state, zip, price,
		"https://cdn.example/videos/"+zpid+".mp4", hash)
	if err != nil {
		t.Fatalf("insert property %s: %v", zpid, err)
	}
}

// requireOneClip asserts that sc contains exactly one current clip.
func requireOneClip(ctx context.Context, t *testing.T, r *Repository, sc Scope) ClipRef {
	t.Helper()
	clips, err := r.ListClips(ctx, sc)
	if err != nil {
		t.Fatalf("list clips %s: %v", sc.Key(), err)
	}
	if len(clips) != 1 {
		t.Fatalf("scope %s has %d current clips, want 1 (the stale render must not be current)", sc.Key(), len(clips))
	}
	return clips[0]
}

func hasClip(clips []ClipRef, id int64) bool {
	for _, c := range clips {
		if c.ID == id {
			return true
		}
	}
	return false
}

func versionNumbers(vs []Version) []int {
	out := make([]int, len(vs))
	for i, v := range vs {
		out[i] = v.Version
	}
	return out
}

// assertVersionEqual checks the array columns survived the round-trip.
func assertVersionEqual(t *testing.T, got, want *Version) {
	t.Helper()
	if got.Key != want.Key || got.Version != want.Version || got.Scope != want.Scope {
		t.Errorf("identity = %s/%d/%s, want %s/%d/%s", got.Key, got.Version, got.Scope, want.Key, want.Version, want.Scope)
	}
	if !got.StartsAt.Equal(want.StartsAt) || !got.EndsAt.Equal(want.EndsAt) {
		t.Errorf("[%s, %s), want [%s, %s)", got.StartsAt, got.EndsAt, want.StartsAt, want.EndsAt)
	}
	if got.StartSeq != want.StartSeq || got.StartItem != want.StartItem {
		t.Errorf("counters = %d/%d, want %d/%d", got.StartSeq, got.StartItem, want.StartSeq, want.StartItem)
	}
	if len(got.ItemIDs) != len(want.ItemIDs) || len(got.ItemMS) != len(want.ItemMS) || len(got.ItemSegs) != len(want.ItemSegs) {
		t.Fatalf("array lengths = %d/%d/%d, want %d/%d/%d",
			len(got.ItemIDs), len(got.ItemMS), len(got.ItemSegs), len(want.ItemIDs), len(want.ItemMS), len(want.ItemSegs))
	}
	for i := range want.ItemIDs {
		if got.ItemIDs[i] != want.ItemIDs[i] || got.ItemMS[i] != want.ItemMS[i] || got.ItemSegs[i] != want.ItemSegs[i] {
			t.Errorf("item %d = %d/%d/%d, want %d/%d/%d", i,
				got.ItemIDs[i], got.ItemMS[i], got.ItemSegs[i], want.ItemIDs[i], want.ItemMS[i], want.ItemSegs[i])
		}
	}
}

// normalise lowercases a city the way ParseScope does, so the scope built
// here matches what a request would produce.
func normalise(city string) string {
	return strings.ToLower(strings.Join(strings.Fields(city), " "))
}
