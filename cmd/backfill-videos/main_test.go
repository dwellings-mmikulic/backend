package main

import (
	"context"
	"errors"
	"testing"

	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
)

// fakeLoader answers with a minimal stored listing and can pull the plug on
// the run at a chosen zpid, the way SIGINT does mid-pass.
type fakeLoader struct {
	interruptAt string
	interrupt   func()
	loaded      []string
}

func (l *fakeLoader) GetByZPID(ctx context.Context, zpid string) (*property.Property, error) {
	if zpid == l.interruptAt && l.interrupt != nil {
		l.interrupt()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.loaded = append(l.loaded, zpid)
	return &property.Property{ZPID: zpid, Address: "1 Main St", Zip: "33950",
		ImageURLs: []string{"https://cdn.example/" + zpid + "/0.jpg"}}, nil
}

// fakeQueue records what it was asked to enqueue and refuses a dead context,
// as the real repository does.
type fakeQueue struct {
	batches [][]string
	err     error
}

func (q *fakeQueue) Enqueue(ctx context.Context, items []workqueue.NewItem) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if q.err != nil {
		return 0, q.err
	}
	var zpids []string
	for _, it := range items {
		zpids = append(zpids, it.ZPID)
	}
	q.batches = append(q.batches, zpids)
	return len(items), nil
}

func (q *fakeQueue) enqueued() []string {
	var out []string
	for _, b := range q.batches {
		out = append(out, b...)
	}
	return out
}

// Ctrl-C used to be reported as a crash: the loop broke on ctx.Err() and the
// final flush then ran on the dead context, so the tool exited 1 with
// "enqueue: context canceled", printed no summary, and said nothing about the
// listings it had already enqueued.
func TestEnqueueRevisits_InterruptStillFlushesAndReports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loader := &fakeLoader{interruptAt: "Z3", interrupt: cancel}
	queue := &fakeQueue{}

	c, err := enqueueRevisits(ctx, loader, queue, []string{"Z1", "Z2", "Z3", "Z4"}, false)

	if err != nil {
		t.Fatalf("enqueueRevisits = %v, want no error: an interrupt is not a failure", err)
	}
	if got := queue.enqueued(); len(got) != 2 || got[0] != "Z1" || got[1] != "Z2" {
		t.Errorf("enqueued %v, want [Z1 Z2]: the batch in hand must still be flushed", got)
	}
	if c.offered != 2 || c.enqueued != 2 || c.failed != 0 {
		t.Errorf("counts = %+v, want 2 offered, 2 enqueued, 0 failed", c)
	}
	if got := loader.loaded; len(got) != 2 {
		t.Errorf("loaded %v, want the pass to stop at the interrupt", got)
	}
}

// The interrupt landing inside GetByZPID is not that listing's fault and must
// not be reported as one.
func TestEnqueueRevisits_InterruptDuringALoadIsNotALoadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loader := &fakeLoader{interruptAt: "Z2", interrupt: cancel}
	queue := &fakeQueue{}

	c, err := enqueueRevisits(ctx, loader, queue, []string{"Z1", "Z2", "Z3"}, false)

	if err != nil {
		t.Fatalf("enqueueRevisits = %v, want no error", err)
	}
	if c.failed != 0 {
		t.Errorf("failed = %d, want 0: the cancelled load is the signal, not a bad row", c.failed)
	}
	if got := queue.enqueued(); len(got) != 1 || got[0] != "Z1" {
		t.Errorf("enqueued %v, want [Z1]", got)
	}
}

// A real enqueue failure, with the context still alive, is still fatal.
func TestEnqueueRevisits_EnqueueErrorIsReturned(t *testing.T) {
	queue := &fakeQueue{err: errors.New("db down")}

	if _, err := enqueueRevisits(context.Background(), &fakeLoader{}, queue, []string{"Z1"}, false); err == nil {
		t.Fatal("want the enqueue error returned")
	}
}

// Items PostgreSQL refuses are left out, not retried: the rest of the batch is
// enqueued and the run carries on.
func TestEnqueueRevisits_UnstorableItemsAreLeftOut(t *testing.T) {
	queue := &fakeQueue{err: workqueue.ErrUnstorable}

	c, err := enqueueRevisits(context.Background(), &fakeLoader{}, queue, []string{"Z1"}, false)

	if err != nil {
		t.Fatalf("enqueueRevisits = %v, want no error for unstorable items", err)
	}
	if c.offered != 1 || c.enqueued != 0 {
		t.Errorf("counts = %+v, want 1 offered and 0 enqueued", c)
	}
}

func TestEnqueueRevisits_DryRunTouchesNothing(t *testing.T) {
	queue := &fakeQueue{}

	c, err := enqueueRevisits(context.Background(), &fakeLoader{}, queue, []string{"Z1", "Z2"}, true)

	if err != nil {
		t.Fatal(err)
	}
	if len(queue.batches) != 0 {
		t.Errorf("a dry run enqueued %v", queue.batches)
	}
	if c.offered != 2 || c.enqueued != 0 {
		t.Errorf("counts = %+v, want 2 offered and 0 enqueued", c)
	}
}
