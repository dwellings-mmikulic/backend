package viewer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
)

// Skipped unless TEST_DATABASE_URL points at a database it may write to.
func TestRepository_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the viewer repository integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE viewer_heartbeats, viewer_last_channel, viewer_clients`); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(pool)

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	a, b := ID{1}, ID{2}
	roku := Client{IP: "99.178.140.144", UserAgent: "Roku/DVP-15.3"}
	beats := []Heartbeat{
		{a, "us", now.Add(-40 * time.Hour), roku},
		{a, "us", now.Add(-time.Minute), roku},
		{a, "state:tx", now, roku}, // latest → last channel
		{b, "us", now.Add(-5 * time.Hour), Client{}},
	}
	if err := repo.InsertHeartbeats(ctx, beats); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertHeartbeats(ctx, beats[:1]); err != nil { // duplicate is fine
		t.Fatal(err)
	}
	if ch, ok, err := repo.LastChannel(ctx, a, now.Add(-time.Hour)); err != nil || !ok || ch != "state:tx" {
		t.Errorf("last channel a = %q %v %v, want state:tx", ch, ok, err)
	}
	if _, ok, _ := repo.LastChannel(ctx, b, now.Add(-time.Hour)); ok {
		t.Error("b was seen 5h ago; since=1h must not match")
	}
	// An older batch must not roll last_channel backwards.
	if err := repo.InsertHeartbeats(ctx, []Heartbeat{{a, "zip:77494", now.Add(-10 * time.Minute), roku}}); err != nil {
		t.Fatal(err)
	}
	if ch, _, _ := repo.LastChannel(ctx, a, now.Add(-time.Hour)); ch != "state:tx" {
		t.Errorf("older batch rolled last channel back to %q", ch)
	}

	s, err := repo.Stats(ctx, "us", now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Concurrent != 1 || s.Unique24h != 2 || s.Unique7d != 2 {
		t.Errorf("stats = %+v, want concurrent 1, 24h 2, 7d 2", s)
	}

	// One row per (ip, user agent, channel) with the seen range; b had no
	// client address, so it has no row.
	cs, err := repo.Clients(ctx, now.Add(-48*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]time.Time{
		"state:tx":  {now, now},
		"zip:77494": {now.Add(-10 * time.Minute), now.Add(-10 * time.Minute)},
		"us":        {now.Add(-40 * time.Hour), now.Add(-time.Minute)},
	}
	if len(cs) != len(want) || cs[0].Channel != "state:tx" {
		t.Fatalf("clients = %+v", cs)
	}
	for _, c := range cs {
		w := want[c.Channel]
		if c.IP != roku.IP || c.UserAgent != roku.UserAgent || !c.FirstSeen.Equal(w[0]) || !c.LastSeen.Equal(w[1]) {
			t.Errorf("client %+v, want seen %v–%v", c, w[0], w[1])
		}
	}
	if cs, _ := repo.Clients(ctx, now.Add(-2*time.Minute), 1); len(cs) != 1 {
		t.Errorf("limit 1 = %d rows", len(cs))
	}

	n, err := repo.Purge(ctx, now.Add(-30*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("purge = %d, %v; want 1 row", n, err)
	}
	// The us row was last seen a minute ago, so it survives the purge.
	if cs, _ := repo.Clients(ctx, now.Add(-48*time.Hour), 10); len(cs) != 3 {
		t.Errorf("after purge: %d client rows, want 3", len(cs))
	}
	if _, err := repo.Purge(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if cs, _ := repo.Clients(ctx, now.Add(-48*time.Hour), 10); len(cs) != 0 {
		t.Errorf("purge must drop client rows: %+v", cs)
	}
}
