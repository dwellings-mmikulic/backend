package viewer

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Heartbeat says a viewer polled a channel during a minute.
type Heartbeat struct {
	Viewer  ID
	Channel string // linear channel key, e.g. "us", "zip:77494"
	Minute  time.Time
}

// Stats summarises a channel's audience.
type Stats struct {
	Concurrent int `json:"concurrent"` // distinct viewers in the last 2 minutes
	Unique24h  int `json:"unique_24h"`
	Unique7d   int `json:"unique_7d"`
}

// Store persists heartbeats.
type Store interface {
	// InsertHeartbeats adds rows, ignoring ones already present, and
	// updates each viewer's last channel.
	InsertHeartbeats(ctx context.Context, hb []Heartbeat) error
	// LastChannel is the channel the viewer last polled, if seen since.
	LastChannel(ctx context.Context, id ID, since time.Time) (channel string, ok bool, err error)
	// Stats is the audience of a channel at now.
	Stats(ctx context.Context, channel string, now time.Time) (Stats, error)
	// Purge deletes heartbeats older than before.
	Purge(ctx context.Context, before time.Time) (int64, error)
}

// Options tune the Recorder. Zero values take the defaults shown.
type Options struct {
	FlushInterval time.Duration // 30s
	PurgeInterval time.Duration // 1h
	Retention     time.Duration // 30 days
	MaxPending    int           // 100k distinct (viewer, channel, minute) rows
}

func (o Options) withDefaults() Options {
	if o.FlushInterval <= 0 {
		o.FlushInterval = 30 * time.Second
	}
	if o.PurgeInterval <= 0 {
		o.PurgeInterval = time.Hour
	}
	if o.Retention <= 0 {
		o.Retention = 30 * 24 * time.Hour
	}
	if o.MaxPending <= 0 {
		o.MaxPending = 100_000
	}
	return o
}

// Recorder collects heartbeats in memory and writes them in batches. It is
// safe for concurrent use.
type Recorder struct {
	store Store
	opts  Options
	log   *slog.Logger

	mu      sync.Mutex
	pending map[Heartbeat]struct{}
}

// NewRecorder creates a Recorder. log may be nil.
func NewRecorder(store Store, opts Options, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{store: store, opts: opts.withDefaults(), log: log, pending: map[Heartbeat]struct{}{}}
}

// Record notes that id polled channel at t. Repeats within the same minute
// collapse into one row. When the buffer is full (the store is unreachable
// for a long time) new beats are dropped rather than growing memory.
func (r *Recorder) Record(id ID, channel string, t time.Time) {
	hb := Heartbeat{Viewer: id, Channel: channel, Minute: t.UTC().Truncate(time.Minute)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending[hb]; ok {
		return
	}
	if len(r.pending) >= r.opts.MaxPending {
		return
	}
	r.pending[hb] = struct{}{}
}

// Pending is the number of buffered rows.
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// Flush writes the buffer. On failure the rows stay buffered for the next
// attempt (the store ignores duplicates, so a partial write is harmless).
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return nil
	}
	batch := make([]Heartbeat, 0, len(r.pending))
	for hb := range r.pending {
		batch = append(batch, hb)
	}
	r.mu.Unlock()

	if err := r.store.InsertHeartbeats(ctx, batch); err != nil {
		return err
	}
	r.mu.Lock()
	for _, hb := range batch {
		delete(r.pending, hb)
	}
	r.mu.Unlock()
	return nil
}

// Run flushes and purges on their intervals until ctx is done, then does a
// final flush so a shutdown loses nothing.
func (r *Recorder) Run(ctx context.Context) {
	flush := time.NewTicker(r.opts.FlushInterval)
	purge := time.NewTicker(r.opts.PurgeInterval)
	defer flush.Stop()
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := r.Flush(fctx); err != nil {
				r.log.Warn("viewer heartbeats: final flush failed", "error", err, "pending", r.Pending())
			}
			cancel()
			return
		case <-flush.C:
			if err := r.Flush(ctx); err != nil {
				r.log.Warn("viewer heartbeats: flush failed", "error", err, "pending", r.Pending())
			}
		case <-purge.C:
			before := time.Now().Add(-r.opts.Retention)
			if n, err := r.store.Purge(ctx, before); err != nil {
				r.log.Warn("viewer heartbeats: purge failed", "error", err)
			} else if n > 0 {
				r.log.Info("viewer heartbeats purged", "rows", n, "before", before)
			}
		}
	}
}
