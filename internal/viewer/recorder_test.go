package viewer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu     sync.Mutex
	beats  []Heartbeat
	purged []time.Time
	fail   error
}

func (f *fakeStore) InsertHeartbeats(_ context.Context, hb []Heartbeat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.beats = append(f.beats, hb...)
	return nil
}
func (f *fakeStore) Purge(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged = append(f.purged, before)
	return 0, nil
}
func (f *fakeStore) LastChannel(context.Context, ID, time.Time) (string, bool, error) {
	return "", false, nil
}
func (f *fakeStore) Stats(context.Context, string, time.Time) (Stats, error) { return Stats{}, nil }
func (f *fakeStore) Clients(context.Context, time.Time, int) ([]ClientSeen, error) {
	return nil, nil
}

func TestRecorder_DedupesPerMinuteAndFlushes(t *testing.T) {
	st := &fakeStore{}
	rec := NewRecorder(st, Options{}, nil)
	t0 := time.Date(2026, 8, 26, 12, 0, 5, 0, time.UTC)
	id := ID{1}
	rec.Record(id, "us", t0, Client{})
	rec.Record(id, "us", t0.Add(20*time.Second), Client{}) // same minute → one row
	rec.Record(id, "us", t0.Add(70*time.Second), Client{}) // next minute
	rec.Record(ID{2}, "state:tx", t0, Client{})
	if n := rec.Pending(); n != 3 {
		t.Fatalf("pending = %d, want 3", n)
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.beats) != 3 || rec.Pending() != 0 {
		t.Fatalf("flushed %d rows, pending %d", len(st.beats), rec.Pending())
	}
	for _, b := range st.beats {
		if b.Minute.Second() != 0 || b.Minute.Nanosecond() != 0 {
			t.Errorf("minute not truncated: %v", b.Minute)
		}
	}
}

func TestRecorder_FlushFailureKeepsPending(t *testing.T) {
	st := &fakeStore{fail: errors.New("db down")}
	rec := NewRecorder(st, Options{}, nil)
	rec.Record(ID{1}, "us", time.Now(), Client{})
	if err := rec.Flush(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if rec.Pending() != 1 {
		t.Errorf("pending after failed flush = %d, want 1 (retry next time)", rec.Pending())
	}
	st.mu.Lock()
	st.fail = nil
	st.mu.Unlock()
	if err := rec.Flush(context.Background()); err != nil || len(st.beats) != 1 {
		t.Errorf("retry: err=%v rows=%d", err, len(st.beats))
	}
}

func TestRecorder_PendingCap(t *testing.T) {
	rec := NewRecorder(&fakeStore{}, Options{MaxPending: 2}, nil)
	now := time.Now()
	rec.Record(ID{1}, "us", now, Client{})
	rec.Record(ID{2}, "us", now, Client{})
	rec.Record(ID{3}, "us", now, Client{}) // dropped, not grown without bound
	if rec.Pending() != 2 {
		t.Errorf("pending = %d, want 2", rec.Pending())
	}
}

func TestRecorder_RunFlushesAndPurges(t *testing.T) {
	st := &fakeStore{}
	rec := NewRecorder(st, Options{FlushInterval: 10 * time.Millisecond, PurgeInterval: 15 * time.Millisecond, Retention: 24 * time.Hour}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rec.Run(ctx); close(done) }()
	rec.Record(ID{1}, "us", time.Now(), Client{})
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.beats) != 1 {
		t.Errorf("run did not flush: %d rows", len(st.beats))
	}
	if len(st.purged) == 0 {
		t.Error("run did not purge")
	} else if d := time.Since(st.purged[0]); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("purge cutoff off: %v ago", d)
	}
}

func TestRecorder_KeepsClient(t *testing.T) {
	st := &fakeStore{}
	rec := NewRecorder(st, Options{}, nil)
	c := Client{IP: "99.178.140.144", UserAgent: "Roku/DVP-15.3"}
	rec.Record(ID{1}, "us", time.Now(), c)
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.beats) != 1 || st.beats[0].Client != c {
		t.Errorf("beats = %+v, want client %+v", st.beats, c)
	}
}
