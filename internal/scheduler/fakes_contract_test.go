package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/workqueue"
	"github.com/dwellingtw/backend/internal/zipcode"
)

// The dispatcher and breaker tests are only as good as the queue they run
// against, so the fake's own rules are pinned here: the ones the real
// listing_queue has (attempts spent at claim time, available_at, fencing
// tokens, dead rows, revival on re-discovery).
func TestFakeQueue_HonoursTheQueueContract(t *testing.T) {
	clock := newFakeClock()
	q := newFakeQueue(clock.now)
	ctx := context.Background()
	enqueue := func(zpids ...string) int {
		var items []workqueue.NewItem
		for _, z := range zpids {
			items = append(items, workqueue.NewItem{ZPID: z, Payload: []byte(`{}`)})
		}
		n, err := q.Enqueue(ctx, items)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := enqueue("a", "b", "", "a"); n != 2 {
		t.Fatalf("enqueued %d, want 2: empty zpids dropped, duplicates collapse", n)
	}
	if n := enqueue("a"); n != 0 {
		t.Errorf("re-enqueued a live row (%d)", n)
	}

	items, _ := q.Claim(ctx, "me", 1, time.Hour)
	if len(items) != 1 || items[0].ZPID != "a" || items[0].Attempts != 1 {
		t.Fatalf("claim = %+v, want a with attempt 1 (oldest first, limit honoured)", items)
	}
	if d, _ := q.Depth(ctx); d != 1 {
		t.Errorf("depth = %d, want 1: a claimed row is not claimable", d)
	}
	if err := q.Complete(ctx, items[0], "somebody-else"); !errors.Is(err, workqueue.ErrLeaseLost) {
		t.Errorf("complete by another owner = %v, want ErrLeaseLost", err)
	}

	// Fail without refund: attempt spent, hidden until available_at.
	if err := q.Fail(ctx, items[0], "me", "boom", 5*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ctx, items[0], "me", "boom", 0, false); !errors.Is(err, workqueue.ErrLeaseLost) {
		t.Errorf("second completion of one claim = %v, want ErrLeaseLost", err)
	}
	if got, _ := q.Claim(ctx, "me", 5, time.Hour); len(got) != 1 || got[0].ZPID != "b" {
		t.Fatalf("claim during a's backoff = %+v, want only b", got)
	}
	clock.advance(5 * time.Minute)
	again, _ := q.Claim(ctx, "me", 5, time.Hour)
	if len(again) != 1 || again[0].ZPID != "a" || again[0].Attempts != 2 {
		t.Fatalf("claim after the backoff = %+v, want a with attempt 2", again)
	}
	if again[0].Token.Equal(items[0].Token) {
		t.Error("two claims of one row share a token")
	}

	// Release and Fail(refund) give the attempt back.
	if err := q.Release(ctx, again[0], "me", 0); err != nil {
		t.Fatal(err)
	}
	third, _ := q.Claim(ctx, "me", 5, time.Hour)
	if len(third) != 1 || third[0].Attempts != 2 {
		t.Fatalf("claim after a release = %+v, want attempt 2 again", third)
	}
	if err := q.Fail(ctx, third[0], "me", "probe", 0, true); err != nil {
		t.Fatal(err)
	}
	if row, _ := q.row("a"); row.attempts != 1 {
		t.Errorf("attempts after a refunded fail = %d, want 1", row.attempts)
	}

	// An expired lease frees the row, and the old holder's token is void.
	fourth, _ := q.Claim(ctx, "me", 5, time.Hour)
	clock.advance(2 * time.Hour)
	fifth, _ := q.Claim(ctx, "other", 5, time.Hour)
	var reclaimed *workqueue.Item
	for i := range fifth {
		if fifth[i].ZPID == "a" {
			reclaimed = &fifth[i]
		}
	}
	if reclaimed == nil || reclaimed.Attempts != 3 {
		t.Fatalf("claim after lease expiry = %+v, want a re-claimed with attempt 3", fifth)
	}
	if err := q.Complete(ctx, fourth[0], "me"); !errors.Is(err, workqueue.ErrLeaseLost) {
		t.Errorf("complete with an expired, re-claimed lease = %v, want ErrLeaseLost", err)
	}

	// Out of attempts: dead, never offered, revived by re-discovery.
	if err := q.Fail(ctx, *reclaimed, "other", "boom", 0, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Claim(ctx, "me", 5, time.Hour); len(got) != 0 {
		t.Errorf("claimed %+v, want nothing: a is dead and b is held", got)
	}
	if n := enqueue("a"); n != 1 {
		t.Errorf("re-enqueue of a dead row = %d, want 1 (revived)", n)
	}
	if row, _ := q.row("a"); row.attempts != 0 {
		t.Errorf("revived row attempts = %d, want 0", row.attempts)
	}
}

func TestFakeZips_HonoursTheRotationContract(t *testing.T) {
	clock := newFakeClock()
	z := newFakeZips(clock.now, "11111", "22222")
	ctx := context.Background()

	c1, _ := z.Claim(ctx, "me", zipLease)
	c2, _ := z.Claim(ctx, "me", zipLease)
	if c1 == nil || c2 == nil || c1.Zip != "11111" || c2.Zip != "22222" {
		t.Fatalf("claims = %+v %+v, want 11111 then 22222: a held ZIP is never offered twice", c1, c2)
	}
	if c3, _ := z.Claim(ctx, "me", zipLease); c3 != nil {
		t.Errorf("third claim = %+v, want nil", c3)
	}

	until := clock.now().Add(time.Hour)
	if err := z.Defer(ctx, *c1, "me", until, 4); err != nil {
		t.Fatal(err)
	}
	if err := z.MarkSearched(ctx, *c1, "me", 1); !errors.Is(err, zipcode.ErrLeaseLost) {
		t.Errorf("second completion of one claim = %v, want ErrLeaseLost", err)
	}
	if c, _ := z.Claim(ctx, "me", zipLease); c != nil {
		t.Errorf("claimed %+v while deferred", c)
	}
	clock.advance(time.Hour)
	c, _ := z.Claim(ctx, "me", zipLease)
	if c == nil || c.Zip != "11111" || c.ResumePage != 4 {
		t.Fatalf("claim after the deferral = %+v, want 11111 resuming at page 4", c)
	}

	// Four failures keep the place in the rotation; the fifth pushes it back.
	for i := 1; i <= zipcode.MaxFailures; i++ {
		pushed, err := z.Fail(ctx, *c, "me", clock.now(), 4)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == zipcode.MaxFailures; pushed != want {
			t.Fatalf("failure %d: pushedBack = %v, want %v", i, pushed, want)
		}
		if !pushed {
			if c, _ = z.Claim(ctx, "me", zipLease); c == nil || c.Zip != "11111" {
				t.Fatalf("failure %d: claim = %+v, want 11111 again", i, c)
			}
		}
	}
	if got := z.markedZips(); !reflect.DeepEqual(got, map[string]int{"11111": 0}) {
		t.Errorf("marked = %v, want the pushed-back ZIP stamped searched", got)
	}
}
