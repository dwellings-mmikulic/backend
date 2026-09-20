package scheduler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
	"github.com/dwellingtw/backend/internal/zillow"
	"github.com/dwellingtw/backend/internal/zipcode"
)

// The fleet tests run eight Schedulers side by side against one real
// PostgreSQL, the way the production fleet runs ten boxes against one: real
// ZIP claims, a real listing queue, a real budget ledger and real property
// rows, one real zillow.Client per instance talking to a fake provider that
// counts every request, a fake CDN and a fake renderer. They prove the goal of
// the multi-instance design end to end: no ZIP searched twice, no listing
// rendered twice, no API budget spent twice, no instance corrupting another's
// work — and the budget binding the whole fleet at once.
//
// They are skipped unless TEST_DATABASE_URL points at a database whose role may
// create schemas in it:
//
//	TEST_DATABASE_URL=postgres://dwellings:dwellings@127.0.0.1:55432/mw_sched?sslmode=disable \
//	    go test -race -count=1 -run Fleet ./internal/scheduler/
//
// The ZIP rotation, the listing queue and the ledger are global tables, so each
// test builds the whole schema again inside a PostgreSQL schema of its own
// (search_path) and drops it afterwards, exactly as the workqueue integration
// tests do.

const (
	fleetInstances = 8
	// fleetAPIPath is the provider's base path. zillow.Client derives the
	// /usage api_id from it, so it has to look like the real one.
	fleetAPIPath = "/realtime-zillow-data"
	// fleetSchedule is production's CRON_SCHEDULE: two 12 h budget windows a
	// day, at 00:00 and 12:00 UTC.
	fleetSchedule = "0 */12 * * *"
	fleetWindow   = 12 * time.Hour
	fleetCDN      = "https://cdn.example/"
)

var (
	fleetRunID = strconv.FormatUint(rand.Uint64(), 36)
	fleetSeq   atomic.Int64
)

// fleetDB returns a pool whose every connection resolves unqualified names in
// a fresh schema holding the whole migrated schema, dropped at cleanup, and the
// DSN that pool was opened with.
func fleetDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the fleet integration tests")
	}
	// Bounded, so that a database that is not answering fails the test here
	// rather than hanging the package.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	schema := "fleet_" + fleetRunID + "_" + strconv.FormatInt(fleetSeq.Add(1), 36)
	if err := fleetExec(ctx, dsn, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create the test's schema: %v", err)
	}
	// Registered first so it runs last: the pool must be closed before the
	// schema can be dropped.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := fleetExec(ctx, dsn, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("cleanup: drop schema %s: %v", schema, err)
		}
	})

	// Eight instances share one pool here, where production gives each box its
	// own: a dozen connections is plenty for their short statements, and the
	// database this runs against has other tenants.
	scoped := fleetSearchPath(dsn, schema)
	pool, err := db.Connect(ctx, scoped, 12)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	var current string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil || current != schema {
		t.Fatalf("current_schema() = %q, %v; want the test's own %q", current, err, schema)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, scoped
}

func fleetExec(ctx context.Context, dsn, sql string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, sql)
	return err
}

func fleetSearchPath(dsn, schema string) string {
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// --- fixture: what the fake provider knows ---

// fleetFixture is the provider's world: ZIPs with 0..4 pages of 3..5
// listings each. zpids are unique per ZIP, except for the overlaps real
// searches have: a few listings show up in neighbouring ZIPs too (one in three
// of them), and one shows up on two pages of the same ZIP.
type fleetFixture struct {
	zips      []string
	pages     map[string][][]string // zip → page-1 → zpids
	pop       map[string]int
	home      map[string]string // zpid → the ZIP it belongs to
	photoBase string
}

func fleetZipCode(i int) string { return fmt.Sprintf("7%04d", i) }

func newFleetFixture(n int, photoBase string) *fleetFixture {
	fx := &fleetFixture{
		pages:     map[string][][]string{},
		pop:       map[string]int{},
		home:      map[string]string{},
		photoBase: photoBase,
	}
	for i := range n {
		zip := fleetZipCode(i)
		fx.zips = append(fx.zips, zip)
		pages := make([][]string, i%5)
		for p := range pages {
			for j := range 3 + (i+p)%3 {
				zpid := fmt.Sprintf("%s-%d-%d", zip, p+1, j)
				pages[p] = append(pages[p], zpid)
				fx.home[zpid] = zip
			}
		}
		fx.pages[zip] = pages
		// Densest first: the rotation claims most populous first, so the
		// first claims of the fleet are the ZIPs with the most pages.
		fx.pop[zip] = len(pages)*1000 + n - i
	}
	fx.share(1, 1, 0, 2, 2)   // a 1-page ZIP's listing on a 2-page ZIP's last page
	fx.share(3, 2, 1, 8, 3)   // between the two 3-page ZIPs
	fx.share(4, 3, 1, 9, 1)   // between the two densest ZIPs, claimed side by side
	fx.share(9, 1, 2, 9, 3)   // on two pages of the same ZIP
	fx.share(14, 4, 0, 19, 4) // one listing in three ZIPs …
	fx.share(14, 4, 0, 24, 1) // … all of them among the first claimed
	return fx
}

// share puts zip #owner's listing (page, j) on zip #target's targetPage too.
// Overlaps that name a ZIP the fixture does not have are left out.
func (fx *fleetFixture) share(owner, page, j, target, targetPage int) {
	if owner >= len(fx.zips) || target >= len(fx.zips) {
		return
	}
	zpid := fmt.Sprintf("%s-%d-%d", fx.zips[owner], page, j)
	tz := fx.zips[target]
	if _, ok := fx.home[zpid]; !ok || targetPage > len(fx.pages[tz]) {
		panic(fmt.Sprintf("fleet fixture: bad overlap %s → %s page %d", zpid, tz, targetPage))
	}
	fx.pages[tz][targetPage-1] = append(fx.pages[tz][targetPage-1], zpid)
}

// distinct is every zpid the provider serves, sorted.
func (fx *fleetFixture) distinct() []string {
	out := make([]string, 0, len(fx.home))
	for zpid := range fx.home {
		out = append(out, zpid)
	}
	sort.Strings(out)
	return out
}

// requests is what a complete search of zip costs: every page of listings
// plus the empty one that ends it.
func (fx *fleetFixture) requests(zip string) int { return len(fx.pages[zip]) + 1 }

func (fx *fleetFixture) totalRequests() int {
	n := 0
	for _, zip := range fx.zips {
		n += fx.requests(zip)
	}
	return n
}

// served is how many listings a complete search of zip returns, a listing on
// two of its pages counted twice.
func (fx *fleetFixture) served(zip string) int {
	n := 0
	for _, page := range fx.pages[zip] {
		n += len(page)
	}
	return n
}

// listing is zpid's search result. It depends on the zpid alone, so a listing
// found through two ZIPs is the very same listing in both.
func (fx *fleetFixture) listing(zpid string) map[string]any {
	h := fnv.New32a()
	_, _ = h.Write([]byte(zpid))
	n := int(h.Sum32() % 100000)
	return map[string]any{
		"zpid":          zpid,
		"price":         150000 + n%900*1000,
		"detailUrl":     "/homedetails/" + zpid + "_zpid/",
		"streetAddress": fmt.Sprintf("%d Fleet St", 1+n%999),
		"city":          "Fleetville",
		"state":         "TX",
		"zipcode":       fx.home[zpid],
		"livingArea":    900 + n%2000,
		"lotAreaValue":  5000,
		"lotAreaUnit":   "sqft",
		"bedrooms":      1 + n%5,
		"bathrooms":     1 + float64(n%4)/2,
		"carouselPhotosComposable": map[string]any{
			"baseUrl": fx.photoBase + "/photos/{photoKey}.jpg",
			"photoData": []any{
				map[string]any{"photoKey": zpid + "-a"},
				map[string]any{"photoKey": zpid + "-b"},
			},
		},
	}
}

// --- the fake provider ---

type fleetPage struct {
	zip  string
	page int
}

type fleetHit struct {
	zip   string
	page  int
	phase int64
}

// fleetAPI is the fake OpenWebNinja: /usage with a healthy quota, /search
// paging the fixture (an empty page ends a ZIP), /property-details with a
// small valid record. Every paid request is counted under the mutex before it
// is answered.
type fleetAPI struct {
	fx  *fleetFixture
	srv *httptest.Server
	// phase tags every search request, so a test can tell which budget
	// window a page was bought in.
	phase atomic.Int64

	mu      sync.Mutex
	search  map[fleetPage]int
	hits    []fleetHit // search requests in arrival order
	details map[string]int
	usage   int
	stray   []string // anything the fixture does not serve
}

func newFleetAPI(t *testing.T, fx *fleetFixture) *fleetAPI {
	t.Helper()
	a := &fleetAPI{fx: fx, search: map[fleetPage]int{}, details: map[string]int{}}
	a.srv = httptest.NewServer(a)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fleetAPI) baseURL() string { return a.srv.URL + fleetAPIPath }

func fleetJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (a *fleetAPI) refuse(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.stray = append(a.stray, r.URL.String())
	a.mu.Unlock()
	http.Error(w, "not served by the fleet fixture", http.StatusBadRequest)
}

func (a *fleetAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch r.URL.Path {
	case "/usage":
		a.mu.Lock()
		a.usage++
		a.mu.Unlock()
		fleetJSON(w, map[string]any{"status": "OK", "data": map[string]any{
			"status": "ok",
			"quotas": []any{map[string]any{"name": "Requests", "limit": 10000000, "used": 0, "remaining": 10000000}},
		}})

	case fleetAPIPath + "/search":
		zip := q.Get("location")
		page, err := strconv.Atoi(q.Get("page"))
		pages, known := a.fx.pages[zip]
		if !known || err != nil || page < 1 {
			a.refuse(w, r)
			return
		}
		a.mu.Lock()
		a.search[fleetPage{zip, page}]++
		a.hits = append(a.hits, fleetHit{zip: zip, page: page, phase: a.phase.Load()})
		a.mu.Unlock()
		data := []any{}
		if page <= len(pages) {
			for _, zpid := range pages[page-1] {
				data = append(data, a.fx.listing(zpid))
			}
		}
		fleetJSON(w, map[string]any{"status": "OK", "data": data})

	case fleetAPIPath + "/property-details":
		zpid := q.Get("zpid")
		if _, known := a.fx.home[zpid]; !known {
			a.refuse(w, r)
			return
		}
		a.mu.Lock()
		a.details[zpid]++
		a.mu.Unlock()
		fleetJSON(w, map[string]any{"status": "OK", "data": map[string]any{
			"zpid": zpid, "homeType": "SINGLE_FAMILY", "homeStatus": "FOR_SALE",
			"description": "Fleet test listing " + zpid, "yearBuilt": 1994,
			"latitude": 30.27, "longitude": -97.74,
		}})

	default:
		a.refuse(w, r)
	}
}

// searchTotal is the number of search requests received so far.
func (a *fleetAPI) searchTotal() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.hits)
}

type fleetAPISnapshot struct {
	search  map[fleetPage]int
	hits    []fleetHit
	details map[string]int
	stray   []string
}

func (a *fleetAPI) snapshot() fleetAPISnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := fleetAPISnapshot{
		search:  make(map[fleetPage]int, len(a.search)),
		hits:    slices.Clone(a.hits),
		details: make(map[string]int, len(a.details)),
		stray:   slices.Clone(a.stray),
	}
	for k, v := range a.search {
		s.search[k] = v
	}
	for k, v := range a.details {
		s.details[k] = v
	}
	return s
}

func (s fleetAPISnapshot) detailsTotal() int {
	n := 0
	for _, c := range s.details {
		n += c
	}
	return n
}

// pagesOf is the pages requested for zip, in arrival order.
func (s fleetAPISnapshot) pagesOf(zip string) []int {
	var out []int
	for _, h := range s.hits {
		if h.zip == zip {
			out = append(out, h.page)
		}
	}
	return out
}

// --- the fake renderer ---

// fleetRenderer writes a stand-in MP4 after a random 1–5 ms, and counts the
// renders that finished, per zpid.
type fleetRenderer struct {
	mu    sync.Mutex
	done  map[string]int
	calls int
}

func (r *fleetRenderer) Render(ctx context.Context, p *property.Property, imgs []string, _, outPath string) (int, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if len(imgs) == 0 {
		return 0, fmt.Errorf("render zpid=%s: no images", p.ZPID)
	}
	t := time.NewTimer(time.Duration(1+rand.IntN(5)) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if err := os.WriteFile(outPath, []byte("video of "+p.ZPID), 0o644); err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.done[p.ZPID]++
	r.mu.Unlock()
	return 2 * len(imgs), nil
}

func (r *fleetRenderer) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.done))
	for k, v := range r.done {
		out[k] = v
	}
	return out
}

// --- the ZIP source every instance uses ---

type fleetZipEvent struct {
	kind   string // claim, mark, defer, fail, release, second-pass, guard
	zip    string
	inst   int
	resume int
	until  time.Time
	phase  int64
	err    error
}

type fleetZipLog struct {
	phase *atomic.Int64
	mu    sync.Mutex
	evs   []fleetZipEvent
}

func (l *fleetZipLog) add(ev fleetZipEvent) {
	ev.phase = l.phase.Load()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
}

func (l *fleetZipLog) all() []fleetZipEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.evs)
}

// fleetZips is the real zipcode.Repository with every transition recorded,
// and one guard that belongs to the harness, not to the system under test.
//
// Claim is a rotation: once every ZIP has been searched it hands out the
// stalest one again, which in production starts the next nationwide pass. With
// a few dozen ZIPs a pass ends inside the test, so the harness ends it there:
// a claim that lands on a ZIP already stamped searched is handed straight back
// and reported as "nothing to claim". While the claim is held nobody else can
// stamp the ZIP, so the check cannot race, and it never touches a ZIP the pass
// still needs — never-searched ZIPs sort first.
type fleetZips struct {
	repo *zipcode.Repository
	pool *pgxpool.Pool
	inst int
	log  *fleetZipLog
}

func (z *fleetZips) Claim(ctx context.Context, owner string, lease time.Duration) (*zipcode.Claim, error) {
	c, err := z.repo.Claim(ctx, owner, lease)
	if err != nil || c == nil {
		return c, err
	}
	var searched bool
	qerr := z.pool.QueryRow(ctx, `SELECT last_searched_at IS NOT NULL FROM zip_codes WHERE zip = $1`, c.Zip).Scan(&searched)
	if qerr == nil && !searched {
		z.log.add(fleetZipEvent{kind: "claim", zip: c.Zip, inst: z.inst, resume: c.ResumePage})
		return c, nil
	}
	rerr := z.repo.Release(context.WithoutCancel(ctx), *c, owner)
	if qerr != nil {
		z.log.add(fleetZipEvent{kind: "guard", zip: c.Zip, inst: z.inst, err: errors.Join(qerr, rerr)})
		return nil, qerr
	}
	z.log.add(fleetZipEvent{kind: "second-pass", zip: c.Zip, inst: z.inst, err: rerr})
	return nil, rerr
}

func (z *fleetZips) MarkSearched(ctx context.Context, c zipcode.Claim, owner string, listingCount int) error {
	err := z.repo.MarkSearched(ctx, c, owner, listingCount)
	z.log.add(fleetZipEvent{kind: "mark", zip: c.Zip, inst: z.inst, resume: c.ResumePage, err: err})
	return err
}

func (z *fleetZips) Defer(ctx context.Context, c zipcode.Claim, owner string, until time.Time, resumePage int) error {
	err := z.repo.Defer(ctx, c, owner, until, resumePage)
	z.log.add(fleetZipEvent{kind: "defer", zip: c.Zip, inst: z.inst, resume: resumePage, until: until, err: err})
	return err
}

func (z *fleetZips) Fail(ctx context.Context, c zipcode.Claim, owner string, until time.Time, resumePage int) (bool, error) {
	pushed, err := z.repo.Fail(ctx, c, owner, until, resumePage)
	z.log.add(fleetZipEvent{kind: "fail", zip: c.Zip, inst: z.inst, resume: resumePage, until: until, err: err})
	return pushed, err
}

func (z *fleetZips) Release(ctx context.Context, c zipcode.Claim, owner string) error {
	err := z.repo.Release(ctx, c, owner)
	z.log.add(fleetZipEvent{kind: "release", zip: c.Zip, inst: z.inst, resume: c.ResumePage, err: err})
	return err
}

// --- logs ---

// fleetLogs keeps every record at Warn or above, tagged with its instance. A
// healthy fleet run has nothing to warn about: a lost lease, a failed
// transition or a failed listing all log at Warn or Error.
type fleetLogs struct {
	mu   sync.Mutex
	recs []string
}

func (l *fleetLogs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recs)
}

type fleetLogHandler struct {
	store *fleetLogs
	inst  int
}

func (h fleetLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h fleetLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h fleetLogHandler) WithGroup(string) slog.Handler            { return h }

func (h fleetLogHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "inst-%d %s %q", h.inst, r.Level, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.recs = append(h.store.recs, b.String())
	return nil
}

// --- the fleet ---

type fleetInstance struct {
	idx    int
	s      *Scheduler
	ctx    context.Context
	cancel context.CancelFunc
}

type fleetOptions struct {
	zips            int
	budget, details int
	// now replaces every instance's clock; nil keeps the real one.
	now func() time.Time
	// wrap lets a test interpose on instance i's collaborators; cancel shuts
	// that instance down.
	wrap func(i int, cancel context.CancelFunc, d *Deps)
}

type fleet struct {
	pool      *pgxpool.Pool
	dsn       string // the pool's DSN, search_path included
	fx        *fleetFixture
	api       *fleetAPI
	render    *fleetRenderer
	bunny     *fakeUploader
	logs      *fleetLogs
	zipLog    *fleetZipLog
	opts      fleetOptions
	cfg       config.Config // every instance's configuration, bar InstanceID
	ctx       context.Context
	insts     []*fleetInstance
	detailsOn bool
}

func newFleet(t *testing.T, opts fleetOptions) *fleet {
	t.Helper()
	pool, dsn := fleetDB(t)
	photos := jpegServer(t)
	fx := newFleetFixture(opts.zips, photos.URL)
	f := &fleet{
		pool:      pool,
		dsn:       dsn,
		fx:        fx,
		api:       newFleetAPI(t, fx),
		render:    &fleetRenderer{done: map[string]int{}},
		bunny:     &fakeUploader{},
		logs:      &fleetLogs{},
		opts:      opts,
		detailsOn: opts.details > 0,
	}
	f.zipLog = &fleetZipLog{phase: &f.api.phase}

	// The test's own ZIPs, and nothing else: no zipseed.
	zips := make([]string, 0, len(fx.zips))
	pops := make([]int32, 0, len(fx.zips))
	for _, zip := range fx.zips {
		zips = append(zips, zip)
		pops = append(pops, int32(fx.pop[zip]))
	}
	const insert = `
INSERT INTO zip_codes (zip, city, state, population)
SELECT z, 'Fleetville', 'TX', p FROM unnest($1::text[], $2::int[]) AS t(z, p)`
	if _, err := pool.Exec(context.Background(), insert, zips, pops); err != nil {
		t.Fatalf("insert test zip codes: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	f.ctx = ctx

	f.cfg = config.Config{
		CronSchedule:      fleetSchedule,
		ZillowBaseURL:     f.api.baseURL(),
		ImagesEnabled:     true,
		SkipExisting:      true,
		DetailsPerCycle:   opts.details,
		APIBudgetPerCycle: opts.budget,
		QueueHighWater:    0,
		Search:            config.SearchCriteria{HomeStatus: "FOR_SALE"},
		Video:             config.VideoConfig{Enabled: true, SecondsPerPhoto: 2},
		Concurrency:       config.ConcurrencyConfig{Listings: 3, Images: 2},
		HTTPTimeout:       10 * time.Second,
	}
	for i := range fleetInstances {
		cfg := f.cfg
		cfg.InstanceID = fmt.Sprintf("inst-%d", i)
		owner := cfg.InstanceID + "/" + strconv.FormatUint(rand.Uint64(), 36)
		windows, err := budget.ParseWindows(fleetSchedule)
		if err != nil {
			t.Fatalf("parse windows: %v", err)
		}
		ictx, icancel := context.WithCancel(ctx)
		d := Deps{
			Zillow:  zillow.New(f.api.baseURL(), "the-one-fleet-key", cfg.HTTPTimeout),
			Bunny:   f.bunny,
			Repo:    property.NewRepository(pool),
			Zips:    &fleetZips{repo: zipcode.NewRepository(pool), pool: pool, inst: i, log: f.zipLog},
			Queue:   workqueue.NewRepository(pool),
			Ledger:  budget.NewLedger(pool),
			Windows: windows,
			Render:  f.render,
		}
		if opts.wrap != nil {
			opts.wrap(i, icancel, &d)
		}
		s := New(&cfg, d, owner, slog.New(fleetLogHandler{store: f.logs, inst: i}))
		if opts.now != nil {
			s.now = opts.now
		}
		f.insts = append(f.insts, &fleetInstance{idx: i, s: s, ctx: ictx, cancel: icancel})
	}
	return f
}

// round runs RunOnce on every instance that is still up, all at once: they are
// held at a gate until every goroutine exists, so their claims really race.
func (f *fleet) round() {
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for _, in := range f.insts {
		if in.ctx.Err() != nil {
			continue
		}
		wg.Go(func() {
			<-gate
			in.s.RunOnce(in.ctx)
		})
	}
	close(gate)
	wg.Wait()
}

// runUntilFinished runs rounds until the fleet has finished the pass: every ZIP
// searched and unclaimed, the queue empty, every listing stored with a ready
// video (and enriched, when details are on).
func (f *fleet) runUntilFinished(t *testing.T, maxRounds int) *fleetState {
	t.Helper()
	for r := 1; ; r++ {
		f.round()
		st := f.snapshot(t)
		if why := f.unfinished(st); why == "" {
			t.Logf("fleet finished in %d round(s)", r)
			return st
		} else if r == maxRounds || f.ctx.Err() != nil {
			t.Errorf("fleet not finished after %d round(s): %s", r, why)
			return st
		}
	}
}

// unfinished says what is left to do, "" when nothing is.
func (f *fleet) unfinished(st *fleetState) string {
	var left []string
	unsearched := 0
	for _, zip := range f.fx.zips {
		if z := st.zips[zip]; !z.searched || z.claimedBy != nil || z.claimedUntil != nil {
			unsearched++
		}
	}
	if unsearched > 0 {
		left = append(left, fmt.Sprintf("%d ZIPs not searched or still claimed", unsearched))
	}
	if len(st.queue) > 0 {
		left = append(left, fmt.Sprintf("%d queue rows %v", len(st.queue), st.queue[:min(len(st.queue), 5)]))
	}
	if len(st.props) != len(f.fx.home) {
		left = append(left, fmt.Sprintf("%d of %d listings stored", len(st.props), len(f.fx.home)))
	}
	notReady, noDetails := 0, 0
	for _, p := range st.props {
		if p.status != string(property.VideoReady) {
			notReady++
		}
		if f.detailsOn && (!p.detailsFetched || p.detailsClaimed) {
			noDetails++
		}
	}
	if notReady > 0 {
		left = append(left, fmt.Sprintf("%d listings without a ready video", notReady))
	}
	if noDetails > 0 {
		left = append(left, fmt.Sprintf("%d listings not enriched", noDetails))
	}
	return strings.Join(left, "; ")
}

// --- database state ---

type fleetZipRow struct {
	searched     bool
	claimedBy    *string
	claimedUntil *time.Time
	failures     int
	resumePage   int
	listingCount *int
}

type fleetPropRow struct {
	status         string
	videoURL       string
	images         []string
	detailsFetched bool
	detailsClaimed bool
}

type fleetBudgetRow struct {
	window time.Time
	kind   string
	spent  int
}

type fleetState struct {
	zips   map[string]fleetZipRow
	queue  []string
	props  map[string]fleetPropRow
	budget []fleetBudgetRow
}

func (f *fleet) snapshot(t *testing.T) *fleetState {
	t.Helper()
	// Bounded, so that a database the fleet has tied up fails the test with
	// what it was doing instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := &fleetState{zips: map[string]fleetZipRow{}, props: map[string]fleetPropRow{}}

	rows, err := f.pool.Query(ctx, `
SELECT zip, last_searched_at IS NOT NULL, claimed_by, claimed_until, failures, resume_page, last_listing_count
  FROM zip_codes`)
	if err != nil {
		t.Fatalf("read zip_codes: %v", err)
	}
	for rows.Next() {
		var zip string
		var z fleetZipRow
		if err := rows.Scan(&zip, &z.searched, &z.claimedBy, &z.claimedUntil, &z.failures, &z.resumePage, &z.listingCount); err != nil {
			t.Fatalf("scan zip_codes: %v", err)
		}
		st.zips[zip] = z
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read zip_codes: %v", err)
	}

	rows, err = f.pool.Query(ctx, `
SELECT zpid, COALESCE(claimed_by, ''), attempts, COALESCE(last_error, '') FROM listing_queue ORDER BY zpid`)
	if err != nil {
		t.Fatalf("read listing_queue: %v", err)
	}
	for rows.Next() {
		var zpid, by, lastErr string
		var attempts int
		if err := rows.Scan(&zpid, &by, &attempts, &lastErr); err != nil {
			t.Fatalf("scan listing_queue: %v", err)
		}
		st.queue = append(st.queue, fmt.Sprintf("%s(by=%q attempts=%d err=%q)", zpid, by, attempts, lastErr))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read listing_queue: %v", err)
	}

	rows, err = f.pool.Query(ctx, `
SELECT zpid, video_status, COALESCE(video_url, ''), image_urls,
       details_fetched_at IS NOT NULL, details_claimed_until IS NOT NULL
  FROM properties`)
	if err != nil {
		t.Fatalf("read properties: %v", err)
	}
	for rows.Next() {
		var zpid string
		var p fleetPropRow
		if err := rows.Scan(&zpid, &p.status, &p.videoURL, &p.images, &p.detailsFetched, &p.detailsClaimed); err != nil {
			t.Fatalf("scan properties: %v", err)
		}
		st.props[zpid] = p
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read properties: %v", err)
	}

	rows, err = f.pool.Query(ctx, `SELECT window_start, kind, spent FROM api_budget ORDER BY window_start, kind`)
	if err != nil {
		t.Fatalf("read api_budget: %v", err)
	}
	for rows.Next() {
		var b fleetBudgetRow
		if err := rows.Scan(&b.window, &b.kind, &b.spent); err != nil {
			t.Fatalf("scan api_budget: %v", err)
		}
		st.budget = append(st.budget, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read api_budget: %v", err)
	}
	return st
}

// spent is the ledger's count for one window and kind.
func (st *fleetState) spent(window time.Time, kind string) int {
	for _, b := range st.budget {
		if b.window.Equal(window) && b.kind == kind {
			return b.spent
		}
	}
	return 0
}

// --- assertions shared by the tests ---

// fleetReport reports the first few failures of one kind and then only counts
// them: when a fleet test goes wrong it usually goes wrong for hundreds of
// listings at once, and the first few say the same thing as all of them.
type fleetReport struct {
	t    *testing.T
	kind string
	n    int
}

func report(t *testing.T, kind string) *fleetReport {
	t.Helper()
	r := &fleetReport{t: t, kind: kind}
	t.Cleanup(r.summary)
	return r
}

const fleetMaxErrors = 5

func (r *fleetReport) errorf(format string, args ...any) {
	r.t.Helper()
	r.n++
	if r.n <= fleetMaxErrors {
		r.t.Errorf(format, args...)
	}
}

func (r *fleetReport) summary() {
	if r.n > fleetMaxErrors {
		r.t.Errorf("%s: %d failures in all, the first %d are above", r.kind, r.n, fleetMaxErrors)
	}
}

// assertSearchedOnce: every page of every ZIP, the empty page that ends it
// included, was requested exactly once and in page order, and nothing else
// was: no ZIP was searched twice, by two instances or by one, and no page
// already paid for was bought again.
func (f *fleet) assertSearchedOnce(t *testing.T, api fleetAPISnapshot) {
	t.Helper()
	r := report(t, "search requests")
	for _, s := range api.stray {
		r.errorf("request the fake provider does not serve: %s", s)
	}
	for _, zip := range f.fx.zips {
		want := f.fx.requests(zip)
		for p := 1; p <= want; p++ {
			if n := api.search[fleetPage{zip, p}]; n != 1 {
				r.errorf("zip %s page %d was requested %d times, want exactly 1", zip, p, n)
			}
		}
		pages := api.pagesOf(zip)
		for i, p := range pages {
			if p != i+1 {
				r.errorf("zip %s: pages requested in the order %v, want 1..%d once each", zip, pages, want)
				break
			}
		}
	}
	for k, n := range api.search {
		if k.page > f.fx.requests(k.zip) {
			r.errorf("zip %s page %d requested %d times: past the empty page that ends the ZIP", k.zip, k.page, n)
		}
	}
}

// assertRenderedOnce: every distinct listing was rendered exactly once —
// those two ZIPs share and the one on two pages of a ZIP included — its video
// and photos were uploaded exactly once each, and it has exactly one
// properties row, ready, pointing at them.
func (f *fleet) assertRenderedOnce(t *testing.T, st *fleetState) {
	t.Helper()
	r := report(t, "renders, uploads and properties rows")
	distinct := f.fx.distinct()
	done := f.render.snapshot()
	for _, zpid := range distinct {
		if n := done[zpid]; n != 1 {
			r.errorf("zpid %s was rendered %d times, want exactly 1", zpid, n)
		}
	}
	for zpid, n := range done {
		if _, ok := f.fx.home[zpid]; !ok {
			r.errorf("rendered zpid %s (%d times) that the provider never served", zpid, n)
		}
	}

	uploads := map[string]int{}
	for _, p := range f.bunny.paths() {
		uploads[p]++
	}
	for _, zpid := range distinct {
		for _, p := range []string{"videos/" + zpid + ".mp4", "properties/" + zpid + "/0.jpg", "properties/" + zpid + "/1.jpg"} {
			if uploads[p] != 1 {
				r.errorf("%s uploaded %d times, want exactly 1", p, uploads[p])
			}
			delete(uploads, p)
		}
	}
	for p, n := range uploads {
		r.errorf("unexpected upload %s (%d times)", p, n)
	}

	if len(st.props) != len(distinct) {
		r.errorf("properties has %d rows, want one per distinct listing: %d", len(st.props), len(distinct))
	}
	for _, zpid := range distinct {
		p, ok := st.props[zpid]
		if !ok {
			r.errorf("zpid %s has no properties row", zpid)
			continue
		}
		if p.status != string(property.VideoReady) || p.videoURL != fleetCDN+"videos/"+zpid+".mp4" {
			r.errorf("zpid %s: video_status=%q video_url=%q, want ready at %svideos/%s.mp4", zpid, p.status, p.videoURL, fleetCDN, zpid)
		}
		want := []string{fleetCDN + "properties/" + zpid + "/0.jpg", fleetCDN + "properties/" + zpid + "/1.jpg"}
		if !slices.Equal(p.images, want) {
			r.errorf("zpid %s: image_urls=%v, want %v", zpid, p.images, want)
		}
	}
	for zpid := range st.props {
		if _, ok := f.fx.home[zpid]; !ok {
			r.errorf("properties row for zpid %s that the provider never served", zpid)
		}
	}
}

// assertDetailsOnce: every listing was enriched, and its details were paid for
// exactly once.
func (f *fleet) assertDetailsOnce(t *testing.T, st *fleetState, api fleetAPISnapshot) {
	t.Helper()
	r := report(t, "details calls")
	for _, zpid := range f.fx.distinct() {
		if n := api.details[zpid]; n != 1 {
			r.errorf("details of zpid %s were fetched %d times, want exactly 1", zpid, n)
		}
		if p, ok := st.props[zpid]; ok && !p.detailsFetched {
			r.errorf("zpid %s was never enriched", zpid)
		}
	}
}

// assertLedgerIsTheBill: what the ledger holds for every window and kind is
// what the provider received, request for request, and no window went over
// its limit.
func (f *fleet) assertLedgerIsTheBill(t *testing.T, st *fleetState, api fleetAPISnapshot) {
	t.Helper()
	limits := map[string]int{
		budget.KindSearch:  max(0, f.opts.budget-f.opts.details),
		budget.KindDetails: f.opts.details,
	}
	total := map[string]int{}
	for _, b := range st.budget {
		limit, ok := limits[b.kind]
		if !ok {
			t.Errorf("api_budget row of unknown kind %q", b.kind)
			continue
		}
		if b.spent > limit {
			t.Errorf("window %s: %s spent %d, over its limit %d", b.window.UTC().Format(time.RFC3339), b.kind, b.spent, limit)
		}
		total[b.kind] += b.spent
	}
	if got, want := total[budget.KindSearch], len(api.hits); got != want {
		t.Errorf("ledger holds %d search requests, the provider received %d", got, want)
	}
	if got, want := total[budget.KindDetails], api.detailsTotal(); got != want {
		t.Errorf("ledger holds %d details requests, the provider received %d", got, want)
	}
}

// assertSettled: every ZIP is stamped searched with its claim cleared, the
// queue is empty, no details claim is left, no instance failed a listing or
// logged a warning, and no claim transition was refused.
func (f *fleet) assertSettled(t *testing.T, st *fleetState) {
	t.Helper()
	r := report(t, "settled state")
	for _, zip := range f.fx.zips {
		z, ok := st.zips[zip]
		switch {
		case !ok:
			r.errorf("zip %s is gone from zip_codes", zip)
		case !z.searched || z.claimedBy != nil || z.claimedUntil != nil || z.failures != 0 || z.resumePage != 0:
			r.errorf("zip %s: searched=%v claimed_by=%v claimed_until=%v failures=%d resume_page=%d; want searched, unclaimed, clean",
				zip, z.searched, deref(z.claimedBy), deref(z.claimedUntil), z.failures, z.resumePage)
		}
	}
	if len(st.queue) != 0 {
		r.errorf("listing_queue still holds %d rows: %v", len(st.queue), st.queue)
	}
	for zpid, p := range st.props {
		if p.detailsClaimed {
			r.errorf("zpid %s still holds a details claim", zpid)
		}
	}
	for _, in := range f.insts {
		if n := in.s.failed.Load(); n != 0 {
			r.errorf("inst-%d failed %d listings; a healthy fleet fails none", in.idx, n)
		}
		if n := in.s.inFlight.Load(); n != 0 {
			r.errorf("inst-%d still has %d listings in flight after RunOnce returned", in.idx, n)
		}
		if in.ctx.Err() == nil && !in.s.Healthy() {
			r.errorf("inst-%d is unhealthy: its media breaker is open", in.idx)
		}
	}
	for _, ev := range f.zipLog.all() {
		if ev.err != nil {
			r.errorf("zip %s: %s by inst-%d was refused: %v", ev.zip, ev.kind, ev.inst, ev.err)
		}
		if ev.kind == "fail" {
			r.errorf("zip %s failed (inst-%d, resume %d): the provider never errs", ev.zip, ev.inst, ev.resume)
		}
	}
	for _, rec := range f.logs.all() {
		r.errorf("unexpected log line: %s", rec)
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// --- the tests ---

// TestFleet_EightInstancesProcessEverythingExactlyOnce is the branch's goal
// end to end: eight instances race over one database, one ledger and one
// provider key until the pass is done, and every ZIP, page, listing, render
// and details call happened exactly once, with the ledger equal to the bill.
func TestFleet_EightInstancesProcessEverythingExactlyOnce(t *testing.T) {
	probe := newFleetFixture(40, "")
	details := len(probe.home)
	f := newFleet(t, fleetOptions{
		zips:    40,
		details: details,                      // exactly one call per listing
		budget:  details + 10*len(probe.zips), // search: far more than one pass
	})
	t.Logf("fixture: %d ZIPs, %d search requests, %d distinct listings",
		len(f.fx.zips), f.fx.totalRequests(), len(f.fx.home))

	st := f.runUntilFinished(t, 20)
	api := f.api.snapshot()

	f.assertSearchedOnce(t, api)        // 1
	f.assertRenderedOnce(t, st)         // 2 and 3
	f.assertLedgerIsTheBill(t, st, api) // 4
	f.assertDetailsOnce(t, st, api)     // 5
	f.assertSettled(t, st)              // 6

	// Every ZIP went through exactly one claim and one stamp: nobody else
	// held it, nothing was deferred or handed back.
	perZip := map[string][]string{}
	for _, ev := range f.zipLog.all() {
		if ev.kind != "second-pass" {
			perZip[ev.zip] = append(perZip[ev.zip], fmt.Sprintf("%s@inst-%d", ev.kind, ev.inst))
		}
	}
	r := report(t, "ZIP claim trails")
	for _, zip := range f.fx.zips {
		evs := perZip[zip]
		if len(evs) != 2 || !strings.HasPrefix(evs[0], "claim@") || !strings.HasPrefix(evs[1], "mark@") ||
			strings.TrimPrefix(evs[0], "claim@") != strings.TrimPrefix(evs[1], "mark@") {
			r.errorf("zip %s went through %v, want one claim and one stamp by the same instance", zip, evs)
		}
		if z := st.zips[zip]; z.listingCount == nil || *z.listingCount != f.fx.served(zip) {
			r.errorf("zip %s: last_listing_count=%v, want %d", zip, deref(z.listingCount), f.fx.served(zip))
		}
	}

	// The claims really raced: the work was spread over the fleet.
	byInst := map[int]int{}
	for _, ev := range f.zipLog.all() {
		if ev.kind == "claim" {
			byInst[ev.inst]++
		}
	}
	if len(byInst) < fleetInstances/2 {
		t.Errorf("only %d of %d instances searched a ZIP (%v): the claims did not race", len(byInst), fleetInstances, byInst)
	}
	t.Logf("ZIP claims per instance: %v", byInst)
}

// TestFleet_BudgetBindsTheWholeFleet gives eight instances 7 search requests
// per window between them. The provider must see exactly 7 per window, fleet
// wide; a ZIP the budget cut short is deferred to the next window with its
// resume page, not stamped searched; and each new window resumes those ZIPs
// where they stopped, never buying a page twice.
func TestFleet_BudgetBindsTheWholeFleet(t *testing.T) {
	const limit = 7
	// A clock in the past, on a window boundary: every window the test walks
	// through has already ended on the database clock, so a ZIP deferred "to
	// the next window" is claimable at once as far as PostgreSQL is concerned.
	// Only the ledger (keyed by the instances' clock) keeps the fleet off it,
	// which is the stricter test.
	base := time.Now().UTC().Truncate(fleetWindow).Add(-40 * fleetWindow)
	window := func(w int) time.Time { return base.Add(time.Duration(w) * fleetWindow) }
	var clock atomic.Int64
	f := newFleet(t, fleetOptions{
		zips:    12,
		budget:  limit,
		details: 0,
		now:     func() time.Time { return time.Unix(0, clock.Load()).UTC() },
	})
	t.Logf("fixture: %d ZIPs, %d search requests, %d distinct listings",
		len(f.fx.zips), f.fx.totalRequests(), len(f.fx.home))
	if f.fx.totalRequests() <= 2*limit {
		t.Fatalf("fixture too small: %d requests do not need several windows of %d", f.fx.totalRequests(), limit)
	}

	resumeAfterW0 := map[string]int{} // ZIPs the first window cut short → resume page
	var st *fleetState
	for w := 0; ; w++ {
		if w == 30 {
			t.Fatalf("the fleet did not finish in %d windows", w)
		}
		clock.Store(window(w).Add(time.Hour).UnixNano())
		f.api.phase.Store(int64(w))
		before := f.api.searchTotal()

		// Rounds until a whole round buys nothing: the window is spent (or
		// the pass is done), and every round drains what it discovered.
		for r := 1; ; r++ {
			n := f.api.searchTotal()
			f.round()
			if f.api.searchTotal() == n {
				break
			}
			if r == 10 {
				t.Fatalf("window %d: still searching after %d rounds", w, r)
			}
		}
		got := f.api.searchTotal() - before
		st = f.snapshot(t)
		api := f.api.snapshot()

		if spent := st.spent(window(w), budget.KindSearch); spent != got {
			t.Errorf("window %d: ledger says %d search requests, the provider received %d", w, spent, got)
		}
		if got > limit {
			t.Errorf("window %d: the provider received %d search requests, over the fleet-wide limit %d", w, got, limit)
		}

		finished := true
		cutShort := 0
		for _, zip := range f.fx.zips {
			z := st.zips[zip]
			fetched := len(api.pagesOf(zip))
			switch {
			case fetched == f.fx.requests(zip):
				if !z.searched || z.resumePage != 0 || z.claimedBy != nil {
					t.Errorf("window %d: zip %s fully fetched but searched=%v resume_page=%d claimed_by=%v",
						w, zip, z.searched, z.resumePage, deref(z.claimedBy))
				}
			case fetched == 0:
				finished = false
				if z.searched || z.resumePage != 0 || z.claimedBy != nil {
					t.Errorf("window %d: zip %s never fetched but searched=%v resume_page=%d claimed_by=%v",
						w, zip, z.searched, z.resumePage, deref(z.claimedBy))
				}
			default: // cut short by the budget
				finished = false
				cutShort++
				if z.searched || z.resumePage != fetched+1 || z.claimedBy != nil || z.failures != 0 {
					t.Errorf("window %d: zip %s cut short after %d pages: searched=%v resume_page=%d claimed_by=%v failures=%d; want deferred, resume_page %d",
						w, zip, fetched, z.searched, z.resumePage, deref(z.claimedBy), z.failures, fetched+1)
				}
				if z.claimedUntil != nil && z.claimedUntil.After(window(w+1)) {
					t.Errorf("window %d: zip %s hidden until %s, past the next window %s",
						w, zip, z.claimedUntil.UTC(), window(w+1))
				}
				if w == 0 {
					resumeAfterW0[zip] = fetched + 1
				}
			}
		}
		if !finished && got != limit {
			t.Errorf("window %d: work left, yet the fleet stopped after %d of %d requests", w, got, limit)
		}
		t.Logf("window %d: %d search requests, %d ZIPs cut short", w, got, cutShort)

		if w == 0 {
			if got != limit {
				t.Fatalf("first window: the provider received %d search requests, want exactly %d", got, limit)
			}
			if len(resumeAfterW0) == 0 {
				t.Fatalf("first window cut no ZIP short: %d requests over ZIPs costing 1–5 each", got)
			}
			// Each was deferred — to the next window, at the first page not
			// fetched — by the instance whose permit was refused.
			for zip, resume := range resumeAfterW0 {
				deferred := false
				for _, ev := range f.zipLog.all() {
					if ev.zip == zip && ev.kind == "defer" && ev.phase == 0 && ev.err == nil &&
						ev.resume == resume && ev.until.Equal(window(1)) {
						deferred = true
					}
				}
				if !deferred {
					t.Errorf("zip %s cut short in window 0 was not deferred to %s at page %d", zip, window(1), resume)
				}
			}
		}
		if finished {
			break
		}
	}

	api := f.api.snapshot()
	// The ZIPs the first window cut short went on at their resume page.
	for zip, resume := range resumeAfterW0 {
		for _, h := range api.hits {
			if h.zip == zip && h.phase > 0 {
				if h.page != resume {
					t.Errorf("zip %s: first request after window 0 was page %d, want its resume page %d", zip, h.page, resume)
				}
				break
			}
		}
	}
	f.assertSearchedOnce(t, api)
	f.assertRenderedOnce(t, st)
	f.assertLedgerIsTheBill(t, st, api)
	f.assertSettled(t, st)
	for _, ev := range f.zipLog.all() {
		if ev.kind == "defer" && !ev.until.After(window(int(ev.phase))) {
			t.Errorf("zip %s deferred in window %d until %s, not to a later window", ev.zip, ev.phase, ev.until.UTC())
		}
	}
	if n := api.detailsTotal(); n != 0 {
		t.Errorf("details were fetched %d times with DETAILS_PER_CYCLE=0", n)
	}
}

// fleetShutdownSearch shuts its instance down on the third permit of a search,
// i.e. with two pages of the ZIP already fetched and paid for.
type fleetShutdownSearch struct {
	*zillow.Client
	cancel context.CancelFunc
	fired  atomic.Bool

	mu   sync.Mutex
	zip  string
	page int // the first page that was not fetched
}

func (z *fleetShutdownSearch) SearchPages(ctx context.Context, c config.SearchCriteria, startPage int, permit zillow.Permit) (zillow.SearchResult, error) {
	asked := 0
	return z.Client.SearchPages(ctx, c, startPage, func(pctx context.Context) bool {
		asked++
		if asked == 3 && z.fired.CompareAndSwap(false, true) {
			z.mu.Lock()
			z.zip, z.page = c.Location, max(startPage, 1)+2
			z.mu.Unlock()
			z.cancel() // SIGTERM, right before the request would be reserved
			return false
		}
		return permit(pctx)
	})
}

// fleetShutdownRender shuts its instance down in the middle of its first
// render. The breaker boots half-open, so that render is the only listing the
// instance has in flight.
type fleetShutdownRender struct {
	inner  Renderer
	cancel context.CancelFunc
	fired  atomic.Bool

	mu   sync.Mutex
	zpid string
}

func (r *fleetShutdownRender) Render(ctx context.Context, p *property.Property, imgs []string, workDir, outPath string) (int, error) {
	if !r.fired.CompareAndSwap(false, true) {
		return r.inner.Render(ctx, p, imgs, workDir, outPath)
	}
	r.mu.Lock()
	r.zpid = p.ZPID
	r.mu.Unlock()
	r.cancel()
	<-ctx.Done()
	return 0, ctx.Err()
}

// TestFleet_ShutdownOfTwoInstancesLosesNothing shuts one instance down in the
// middle of a search and another in the middle of a render while the other six
// carry on. Afterwards everything is still searched, rendered and enriched
// exactly once, the paid pages were not bought again, and nothing is left
// claimed.
func TestFleet_ShutdownOfTwoInstancesLosesNothing(t *testing.T) {
	probe := newFleetFixture(40, "")
	details := len(probe.home)
	search := &fleetShutdownSearch{}
	render := &fleetShutdownRender{}
	f := newFleet(t, fleetOptions{
		zips:    40,
		details: details,
		budget:  details + 10*len(probe.zips),
		wrap: func(i int, cancel context.CancelFunc, d *Deps) {
			switch i {
			case 0:
				search.Client, search.cancel = d.Zillow.(*zillow.Client), cancel
				d.Zillow = search
			case 1:
				render.inner, render.cancel = d.Render, cancel
				d.Render = render
			}
		},
	})

	st := f.runUntilFinished(t, 20)
	api := f.api.snapshot()

	if !search.fired.Load() {
		t.Fatal("inst-0 was never shut down mid-search: the test proved nothing about it")
	}
	if !render.fired.Load() {
		t.Fatal("inst-1 was never shut down mid-render: the test proved nothing about it")
	}
	search.mu.Lock()
	cutZip, cutPage := search.zip, search.page
	search.mu.Unlock()
	render.mu.Lock()
	cutZpid := render.zpid
	render.mu.Unlock()
	t.Logf("inst-0 stopped searching zip %s before page %d; inst-1 stopped rendering zpid %s", cutZip, cutPage, cutZpid)

	f.assertSearchedOnce(t, api)
	f.assertRenderedOnce(t, st)
	f.assertLedgerIsTheBill(t, st, api)
	f.assertDetailsOnce(t, st, api)
	f.assertSettled(t, st)

	// The ZIP inst-0 was searching went back with its fetched pages kept —
	// deferred to "now" at the first unfetched page — and was finished there
	// by another instance.
	var trail []string
	resumedElsewhere := false
	for _, ev := range f.zipLog.all() {
		if ev.zip != cutZip || ev.kind == "second-pass" {
			continue
		}
		trail = append(trail, fmt.Sprintf("%s@inst-%d(resume=%d)", ev.kind, ev.inst, ev.resume))
		if ev.kind == "claim" && ev.inst != 0 && ev.resume == cutPage {
			resumedElsewhere = true
		}
	}
	if !slices.Contains(trail, fmt.Sprintf("defer@inst-0(resume=%d)", cutPage)) || !resumedElsewhere {
		t.Errorf("zip %s went through %v; want inst-0 to defer it at page %d and another instance to resume it there", cutZip, trail, cutPage)
	}

	for _, in := range f.insts[:2] {
		if in.ctx.Err() == nil {
			t.Errorf("inst-%d is still up", in.idx)
		}
	}
}

// TestFleet_StartedLoopsProcessEverythingExactlyOnce runs the production entry
// point instead of RunOnce: under Start every instance runs its discovery,
// media and details loops (and its status line) side by side, so discovery
// races the media dispatcher and the details loop inside each instance as well
// as across the fleet. The loops' waits are cut to a few milliseconds. Once
// the pass is done every instance is stopped, and Stop has to return.
func TestFleet_StartedLoopsProcessEverythingExactlyOnce(t *testing.T) {
	probe := newFleetFixture(40, "")
	details := len(probe.home)
	f := newFleet(t, fleetOptions{
		zips:    40,
		details: details,
		budget:  details + 10*len(probe.zips),
	})
	// Time runs a thousand times faster: a one-minute idle wait is 50 ms, the
	// 5 s queue poll 5 ms.
	for _, in := range f.insts {
		in.s.sleep = func(ctx context.Context, d time.Duration) { sleepCtx(ctx, min(d/1000, 50*time.Millisecond)) }
	}

	stopped := false
	stopAll := func() {
		if stopped {
			return
		}
		stopped = true
		var wg sync.WaitGroup
		for _, in := range f.insts {
			wg.Go(in.s.Stop)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("Stop did not return: a loop or a listing goroutine is stuck")
		}
	}
	// Runs before the pool is closed and the schema dropped (LIFO), also when
	// an assertion below stops the test.
	t.Cleanup(stopAll)
	for _, in := range f.insts {
		if err := in.s.Start(in.ctx); err != nil {
			t.Fatalf("start inst-%d: %v", in.idx, err)
		}
	}

	// The pass is done when a snapshot finds everything finished; the loops
	// keep running meanwhile, as they would in production.
	deadline := time.Now().Add(60 * time.Second)
	var st *fleetState
	for {
		st = f.snapshot(t)
		why := f.unfinished(st)
		if why == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("fleet not finished after 60 s: %s", why)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	api := f.api.snapshot()
	f.assertSearchedOnce(t, api)
	f.assertRenderedOnce(t, st)
	f.assertLedgerIsTheBill(t, st, api)
	f.assertDetailsOnce(t, st, api)
	f.assertSettled(t, st)

	stopAll()

	// Stopping bought, rendered and dropped nothing. What the ZIP claims look
	// like after a Stop that landed on a claim statement is
	// TestFleet_ShutdownDuringAClaimLeavesNothingClaimed's business.
	after := f.snapshot(t)
	if api2 := f.api.snapshot(); len(api2.hits) != len(api.hits) || api2.detailsTotal() != api.detailsTotal() {
		t.Errorf("provider requests while stopping: search %d → %d, details %d → %d",
			len(api.hits), len(api2.hits), api.detailsTotal(), api2.detailsTotal())
	}
	if len(after.queue) != 0 {
		t.Errorf("listing_queue holds %d rows after Stop: %v", len(after.queue), after.queue)
	}
	for zpid, p := range after.props {
		if p.status != string(property.VideoReady) || !p.detailsFetched || p.detailsClaimed {
			t.Errorf("zpid %s after Stop: %+v", zpid, p)
		}
	}
	for _, in := range f.insts {
		if n := in.s.inFlight.Load(); n != 0 {
			t.Errorf("inst-%d has %d listings in flight after Stop returned", in.idx, n)
		}
	}
}

// TestFleet_ShutdownDuringAClaimLeavesNothingClaimed shuts an instance down
// while one of its claim statements is in flight. A lock on the claimed table
// holds the statement there, which is what a claim looks like from the outside
// when SIGTERM lands during its round trip; the lock is then let go and the
// statement runs its course. The contract of a graceful shutdown is that it
// hands every claim back at once (spec §3.1: "graceful shutdown releases
// immediately"; §3.9: "Context cancel stops claiming"), so afterwards nothing
// may be left claimed — and a listing must not have paid an attempt for it.
//
// What happens in production when the context is cancelled mid-statement: pgx
// hangs up on the connection (DeadlineContextWatcherHandler) and sends
// PostgreSQL a cancel request on a new connection. The server has the whole
// statement already — pgx sent Bind/Execute/Sync in one go, the claim being in
// every running worker's statement cache — so it runs to its implicit commit
// unless the cancel request gets there first. A claim takes about a
// millisecond; a cancel request needs a connection (and a backend fork) of its
// own, so the commit usually wins. The test makes that order deterministic by
// holding the cancel request back until the statement has finished. The
// instance runs on a pool of one connection, warmed up with the claim, so the
// statement it sends is exactly that.
func TestFleet_ShutdownDuringAClaimLeavesNothingClaimed(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		table   string // the table the claim is held up on
		pattern string // the claim statement, as pg_stat_activity shows it
		opts    fleetOptions
		prepare func(t *testing.T, f *fleet, solo *pgxpool.Pool)
		leaked  string // one text row per claim left behind
	}{
		{
			name: "zip", table: "zip_codes", pattern: "%UPDATE zip_codes z%",
			opts: fleetOptions{zips: 3, budget: 100},
			prepare: func(t *testing.T, _ *fleet, solo *pgxpool.Pool) {
				r := zipcode.NewRepository(solo)
				c, err := r.Claim(ctx, "warmup/1", time.Minute)
				if err != nil || c == nil {
					t.Fatalf("warm-up claim: %v, %v", c, err)
				}
				if err := r.Release(ctx, *c, "warmup/1"); err != nil {
					t.Fatalf("warm-up release: %v", err)
				}
			},
			leaked: `SELECT zip || ' claimed_by=' || COALESCE(claimed_by, '<none>') || ' claimed_until=' || COALESCE(claimed_until::text, '<none>')
  FROM zip_codes WHERE claimed_by IS NOT NULL OR claimed_until IS NOT NULL`,
		},
		{
			name: "listing", table: "listing_queue", pattern: "%UPDATE listing_queue q%",
			opts: fleetOptions{zips: 0, budget: 100},
			prepare: func(t *testing.T, f *fleet, solo *pgxpool.Pool) {
				if _, err := workqueue.NewRepository(solo).Claim(ctx, "warmup/1", 1, time.Minute); err != nil {
					t.Fatalf("warm-up claim: %v", err)
				}
				var items []workqueue.NewItem
				for _, zpid := range []string{"leak-1", "leak-2"} {
					payload, err := EncodeListing(&property.Property{
						ZPID: zpid, Address: "1 Leak St", ImageURLs: []string{f.fx.photoBase + "/photos/" + zpid + ".jpg"},
					}, false)
					if err != nil {
						t.Fatal(err)
					}
					items = append(items, workqueue.NewItem{ZPID: zpid, Payload: payload, SourceZip: "70000"})
				}
				if _, err := workqueue.NewRepository(f.pool).Enqueue(ctx, items); err != nil {
					t.Fatalf("enqueue: %v", err)
				}
			},
			leaked: `SELECT zpid || ' claimed_by=' || COALESCE(claimed_by, '<none>') || ' attempts=' || attempts
  FROM listing_queue WHERE claimed_by IS NOT NULL OR claimed_until IS NOT NULL OR attempts > 0`,
		},
		{
			name: "details", table: "properties", pattern: "%UPDATE properties p%",
			opts: fleetOptions{zips: 0, budget: 100, details: 10},
			prepare: func(t *testing.T, f *fleet, solo *pgxpool.Pool) {
				if _, err := property.NewRepository(solo).ClaimMissingDetails(ctx, 1, time.Minute); err != nil {
					t.Fatalf("warm-up claim: %v", err)
				}
				for _, zpid := range []string{"leak-1", "leak-2"} {
					p := &property.Property{ZPID: zpid, Address: "1 Leak St", ImageURLs: []string{}}
					if err := property.NewRepository(f.pool).Upsert(ctx, p); err != nil {
						t.Fatalf("upsert: %v", err)
					}
				}
			},
			leaked: `SELECT zpid || ' details_claimed_until=' || details_claimed_until::text
  FROM properties WHERE details_claimed_until IS NOT NULL`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFleet(t, tc.opts)
			solo, releaseCancels := f.soloPool(t)
			tc.prepare(t, f, solo)
			s, sctx, stop := f.soloInstance(t, solo)

			lock, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Rollback(ctx) }()
			if _, err := lock.Exec(ctx, `LOCK TABLE `+tc.table+` IN EXCLUSIVE MODE`); err != nil {
				t.Fatalf("lock %s: %v", tc.table, err)
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				s.RunOnce(sctx)
			}()
			pid := f.waitBlocked(t, tc.pattern)

			stop() // SIGTERM, with the claim in flight
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
			}
			if err := lock.Rollback(ctx); err != nil { // the claim runs its course
				t.Fatalf("unlock %s: %v", tc.table, err)
			}
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("RunOnce did not return after the shutdown")
			}
			f.waitSettled(t, pid)
			releaseCancels() // the cancel request arrives: too late

			rows, err := f.pool.Query(ctx, tc.leaked)
			if err != nil {
				t.Fatal(err)
			}
			left, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range left {
				t.Errorf("after a graceful shutdown during its claim the instance left %s: nobody holds it, and it stays hidden until the lease runs out", row)
			}
		})
	}
}

// soloPool is a pool of exactly one connection on the fleet's schema, so that
// every statement a solo instance runs goes through the statement cache the
// test warmed up. Cancel requests sent from it are held back until release is
// called.
func (f *fleet) soloPool(t *testing.T) (pool *pgxpool.Pool, release func()) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	dial := cfg.ConnConfig.DialFunc
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &fleetCancelGate{Conn: c, gate: gate}, nil
	}
	pool, err = pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(release) // LIFO: before the pool closes
	return pool, release
}

// pgCancelRequestCode opens a PostgreSQL CancelRequest packet.
const pgCancelRequestCode = 80877102

// fleetCancelGate is a connection that, when it turns out to carry a
// PostgreSQL cancel request, holds that request back until gate is closed.
// pgx writes the whole packet in one Write, the connection's first.
type fleetCancelGate struct {
	net.Conn
	gate    <-chan struct{}
	checked atomic.Bool
}

func (c *fleetCancelGate) Write(p []byte) (int, error) {
	if c.checked.CompareAndSwap(false, true) && len(p) >= 8 && binary.BigEndian.Uint32(p[4:8]) == pgCancelRequestCode {
		<-c.gate
	}
	return c.Conn.Write(p)
}

// soloInstance is one more instance of the fleet, on the given pool, and the
// context that shuts it down.
func (f *fleet) soloInstance(t *testing.T, pool *pgxpool.Pool) (*Scheduler, context.Context, context.CancelFunc) {
	t.Helper()
	cfg := f.cfg
	cfg.InstanceID = "solo"
	windows, err := budget.ParseWindows(fleetSchedule)
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cfg, Deps{
		Zillow:  zillow.New(f.api.baseURL(), "the-one-fleet-key", cfg.HTTPTimeout),
		Bunny:   f.bunny,
		Repo:    property.NewRepository(pool),
		Zips:    zipcode.NewRepository(pool),
		Queue:   workqueue.NewRepository(pool),
		Ledger:  budget.NewLedger(pool),
		Windows: windows,
		Render:  f.render,
	}, "solo/"+strconv.FormatUint(rand.Uint64(), 36), slog.New(fleetLogHandler{store: f.logs, inst: 99}))
	ctx, cancel := context.WithCancel(f.ctx)
	t.Cleanup(cancel)
	return s, ctx, cancel
}

// waitBlocked returns the backend whose statement matching pattern is waiting
// for a lock.
func (f *fleet) waitBlocked(t *testing.T, pattern string) int32 {
	t.Helper()
	const q = `
SELECT pid FROM pg_stat_activity
 WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var pid int32
		err := f.pool.QueryRow(context.Background(), q, pattern).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
	}
	t.Fatalf("no statement like %q ever waited for the lock", pattern)
	return 0
}

// waitSettled waits until backend pid has finished its statement: gone, or
// idle again.
func (f *fleet) waitSettled(t *testing.T, pid int32) {
	t.Helper()
	const q = `SELECT state FROM pg_stat_activity WHERE pid = $1`
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var state *string
		err := f.pool.QueryRow(context.Background(), q, pid).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != nil && *state == "idle") {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("backend %d never finished its statement", pid)
}
