// Package scheduler runs the three worker loops of an instance: discovery
// (search ZIPs, enqueue listings), media (photos, persist, render, segment)
// and details enrichment. PostgreSQL coordinates the fleet: every unit of work
// is taken with an expiring claim, and every paid request is reserved in the
// shared budget ledger first.
package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
	"github.com/dwellingtw/backend/internal/zillow"
	"github.com/dwellingtw/backend/internal/zipcode"
)

// The timing of the loops. Every lease is longer than the deadline of the work
// done under it (spec 3.1): a live worker therefore never works on an expired
// claim, nothing has to renew one, and the margin is what the bookkeeping
// after the work runs in.
const (
	// zipLease is how long a ZIP claim hides the ZIP from the rest of the fleet.
	zipLease = 15 * time.Minute
	// zipDeadline bounds one ZIP's search: up to 20 pages, each with retries
	// that may honour a Retry-After of a minute.
	zipDeadline = 10 * time.Minute
	// listingLease only matters after a hard crash: it is how long the item
	// then stays stuck. A graceful shutdown releases at once.
	listingLease = 65 * time.Minute
	// listingDeadline bounds one listing's photos, render, upload and
	// segmentation. Videos run up to 13 minutes, and several are encoded side
	// by side on the same CPUs.
	listingDeadline = 60 * time.Minute
	// detailsLease covers one details batch; a row that failed for a reason
	// of its own keeps what is left of it as its backoff.
	detailsLease = 10 * time.Minute
	// detailsDeadline bounds one details batch.
	detailsDeadline = 5 * time.Minute
	// detailsBatch is how many rows one details claim takes: small, so a
	// shutdown or an outage hands few rows back and the fleet shares the work.
	detailsBatch = 10
	// bookkeepingTimeout bounds every transition recorded after the work
	// (enqueue, mark, fail, complete, release…). Those run detached from the
	// work's context, so a deadline or a SIGTERM never loses them.
	bookkeepingTimeout = 15 * time.Second
	// queuePoll is how often an idle media loop looks at the queue again.
	queuePoll = 5 * time.Second
	// backpressureWait is how long discovery stands back while the queue is
	// at its high-water mark.
	backpressureWait = 30 * time.Second
	// noZipWait is how long discovery waits when every ZIP is claimed or
	// backing off (or the claim itself failed).
	noZipWait = time.Minute
	// noDetailsWait is how long the details loop waits when no row needs
	// enriching (or the claim itself failed).
	noDetailsWait = time.Minute
	// detailsDisabledWait is the details loop's step while DETAILS_PER_CYCLE
	// is 0: the loop stays up, effectively idle.
	detailsDisabledWait = time.Hour
	// maxWindowSleep caps every "wait for the next window" sleep.
	maxWindowSleep = 10 * time.Minute
	// zipFailBackoff is the least a failed ZIP is hidden for. The next window
	// start is used instead when it is later, which keeps the old "a failed
	// ZIP retries next cycle".
	zipFailBackoff = time.Hour
	// failurePauseBase and failurePauseMax shape the pause a loop takes after
	// k consecutive provider failures: 5 s, 10 s, 20 s, … capped at 5 min.
	failurePauseBase = 5 * time.Second
	failurePauseMax  = 5 * time.Minute
	// listingRetryFirst and listingRetryLater are a failed listing's backoff
	// after its first attempt and after every later one.
	listingRetryFirst = 5 * time.Minute
	listingRetryLater = 30 * time.Minute
	// poisonRetry is the backoff of an item whose payload cannot be decoded.
	// Nothing will ever fix it, so it is only kept out of the way until its
	// attempts run out and it is left dead for inspection.
	poisonRetry = 24 * time.Hour
	// revisitNoVideoDelay is how long a revisit item waits after it was
	// claimed by a worker that does not render video.
	revisitNoVideoDelay = 10 * time.Minute
	// breakerThreshold is how many infrastructure failures in a row open the
	// media breaker. One failure is a bad listing; five in a row is a bad box.
	breakerThreshold = 5
	// breakerBasePause is the breaker's first pause; every further opening in
	// a row doubles it, up to breakerMaxPause, so a box that recovers is back
	// in the fleet within a quarter of an hour.
	breakerBasePause = time.Minute
	breakerMaxPause  = 15 * time.Minute
	// maxErrorLen caps the message kept in listing_queue.last_error. ffmpeg
	// can return pages of stderr, and the column is for a human reading the
	// runbook's "dead rows by error" query.
	maxErrorLen = 1000
	// statusInterval is the period of the "worker status" log line.
	statusInterval = time.Minute
)

// zillowAPI discovers properties, fetches their one-time details record, and
// reports the provider's quota state. Search and details ask the permit before
// every paid HTTP attempt.
type zillowAPI interface {
	SearchPages(ctx context.Context, s config.SearchCriteria, startPage int, permit zillow.Permit) (zillow.SearchResult, error)
	PropertyDetails(ctx context.Context, zpid string, permit zillow.Permit) (*property.Details, []byte, error)
	Usage(ctx context.Context) (*zillow.Usage, error)
}

// zipSource hands out exclusive claims on ZIPs in rotation order
// (zipcode.Repository). Every claim ends in exactly one of MarkSearched,
// Defer, Fail or Release.
type zipSource interface {
	Claim(ctx context.Context, owner string, lease time.Duration) (*zipcode.Claim, error)
	MarkSearched(ctx context.Context, c zipcode.Claim, owner string, listingCount int) error
	Defer(ctx context.Context, c zipcode.Claim, owner string, until time.Time, resumePage int) error
	Fail(ctx context.Context, c zipcode.Claim, owner string, until time.Time, resumePage int) (bool, error)
	Release(ctx context.Context, c zipcode.Claim, owner string) error
}

// listingQueue is the work queue between discovery and the media loop
// (workqueue.Repository). Every claimed item ends in exactly one of Complete,
// Fail or Release.
type listingQueue interface {
	Enqueue(ctx context.Context, items []workqueue.NewItem) (int, error)
	Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]workqueue.Item, error)
	Complete(ctx context.Context, it workqueue.Item, owner string) error
	Fail(ctx context.Context, it workqueue.Item, owner, errMsg string, retryAfter time.Duration, refundAttempt bool) error
	Release(ctx context.Context, it workqueue.Item, owner string, delay time.Duration) error
	Depth(ctx context.Context) (int, error)
}

// budgetLedger is the fleet-wide count of paid requests per budget window
// (budget.Ledger).
type budgetLedger interface {
	TryReserve(ctx context.Context, window time.Time, kind string, limit int) (bool, error)
	Spent(ctx context.Context, window time.Time, kind string) (int, error)
}

// windowClock maps an instant to its budget window (budget.Windows).
type windowClock interface {
	Current(now time.Time) (start, next time.Time)
}

// uploader stores content and returns its public CDN URL.
type uploader interface {
	Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error)
}

// store persists properties, their video state and their details claims.
type store interface {
	Exists(ctx context.Context, zpid string) (bool, error)
	NeedsVideo(ctx context.Context, zpid string) (bool, error)
	Upsert(ctx context.Context, p *property.Property) error
	SetVideoReady(ctx context.Context, zpid, videoURL, contentHash string, durationSecs int) error
	SetVideoFailed(ctx context.Context, zpid string) error
	SetDetails(ctx context.Context, zpid string, d *property.Details, raw []byte) error
	VideoStates(ctx context.Context, zpids []string) (map[string]bool, error)
	ClaimMissingDetails(ctx context.Context, limit int, lease time.Duration) ([]string, error)
	ReleaseDetails(ctx context.Context, zpids []string) error
	FailDetails(ctx context.Context, zpid string) error
}

// Renderer turns a property + local photos into an MP4, returning its duration.
type Renderer interface {
	Render(ctx context.Context, p *property.Property, imagePaths []string, workDir, outPath string) (int, error)
}

// Segmenter cuts a rendered MP4 into HLS segments (hls.Segmenter).
type Segmenter interface {
	Segment(ctx context.Context, mp4Path, outDir string) (hls.Clip, error)
}

// HLSRecorder stores a clip's segment layout for the linear channels
// (linear.Repository).
type HLSRecorder interface {
	SetVideoHLS(ctx context.Context, zpid, contentHash, baseURL string, clip hls.Clip) error
}

// Deps keeps New readable now that there are this many collaborators.
type Deps struct {
	Zillow  zillowAPI
	Bunny   uploader
	Repo    store
	Zips    zipSource
	Queue   listingQueue
	Ledger  budgetLedger
	Windows windowClock
	Render  Renderer // nil when video rendering is disabled
}

// Scheduler owns an instance's worker loops.
type Scheduler struct {
	cfg     *config.Config
	zillow  zillowAPI
	bunny   uploader
	repo    store
	zips    zipSource
	queue   listingQueue
	ledger  budgetLedger
	windows windowClock
	render  Renderer
	owner   string
	http    *http.Client
	log     *slog.Logger

	// now and sleep are the only ways the loops read the clock or wait, so
	// tests drive time instead of sleeping through it.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration)

	segmenter Segmenter // nil: channel segmentation disabled
	hls       HLSRecorder

	breaker *breaker
	gate    quotaGate

	// discoverFailures counts consecutive failed searches. Only the discovery
	// loop's goroutine touches it.
	discoverFailures int
	// detailsFailures is the same for the details loop.
	detailsFailures int

	// Counters since boot, for the status line.
	inFlight, completed, failed atomic.Int64

	// wg tracks every goroutine Start launches, item goroutines included.
	wg sync.WaitGroup

	// lifecycle guards started and cancel, so Stop can race Start safely.
	lifecycle sync.Mutex
	started   bool
	cancel    context.CancelFunc
}

// New creates a Scheduler. owner is the claim owner (INSTANCE_ID plus a
// per-boot nonce, built by main): it is what every claim this instance takes
// is guarded by. d.Render may be nil when video rendering is disabled.
func New(cfg *config.Config, d Deps, owner string, log *slog.Logger) *Scheduler {
	s := &Scheduler{
		cfg:     cfg,
		zillow:  d.Zillow,
		bunny:   d.Bunny,
		repo:    d.Repo,
		zips:    d.Zips,
		queue:   d.Queue,
		ledger:  d.Ledger,
		windows: d.Windows,
		render:  d.Render,
		owner:   owner,
		http:    &http.Client{Timeout: cfg.HTTPTimeout},
		log:     log,
		now:     time.Now,
		sleep:   sleepCtx,
	}
	// Through s.now, not time.Now, so that whoever replaces the scheduler's
	// clock replaces the breaker's with it.
	s.breaker = newBreaker(func() time.Time { return s.now() }, log)
	return s
}

// EnableHLS turns on channel segmentation of newly rendered videos. Call
// before Start.
func (s *Scheduler) EnableHLS(seg Segmenter, rec HLSRecorder) {
	s.segmenter, s.hls = seg, rec
}

// Healthy is false while the media breaker is open: this box keeps failing
// listings for reasons of its own (blocked IP, full disk, broken ffmpeg) and
// has stopped claiming. A ROLE=worker /healthz reports it; nothing restarts
// the box over it, the breaker probes again by itself.
func (s *Scheduler) Healthy() bool {
	return s.breaker.healthy()
}

// sleepCtx waits for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// videoWanted reports whether this instance renders listing videos.
func (s *Scheduler) videoWanted() bool {
	return s.cfg.Video.Enabled && s.render != nil
}

// bookkeeping returns the context every post-work transition runs under
// (enqueue, mark, defer, fail, complete, release, a fetched details record):
// detached from ctx's cancellation and deadline, with a short timeout of its
// own. The work's deadline running out, or a SIGTERM, is exactly when these
// writes matter most, and the lease margin exists to give them time.
func (s *Scheduler) bookkeeping(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
}

// transitionFailed logs a claim transition that was not recorded. A lost
// lease is only a warning: the statement changed nothing, and whoever holds
// the claim now does its bookkeeping.
func (s *Scheduler) transitionFailed(what string, err error, attrs ...any) {
	attrs = append(attrs, "error", err)
	if errors.Is(err, zipcode.ErrLeaseLost) || errors.Is(err, workqueue.ErrLeaseLost) {
		s.log.Warn(what+": lease lost, nothing recorded", attrs...)
		return
	}
	s.log.Error(what+" failed", attrs...)
}
