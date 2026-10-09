package photopurge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const cdn = "https://cdn.example"

// fakeStorage is bunny.Client in memory: ObjectPath accepts this CDN's URLs
// and Delete records what it was asked to remove.
type fakeStorage struct {
	mu        sync.Mutex
	deleted   []string
	failPaths map[string]bool // Delete of these returns an error
	cur, peak atomic.Int64
	block     chan struct{} // when set, Delete waits on it (to measure concurrency)
}

func (s *fakeStorage) ObjectPath(cdnURL string) (string, error) {
	rest, ok := strings.CutPrefix(cdnURL, cdn+"/")
	if !ok || rest == "" {
		return "", fmt.Errorf("not an object of %s: %q", cdn, cdnURL)
	}
	return rest, nil
}

func (s *fakeStorage) Delete(ctx context.Context, path string) error {
	n := s.cur.Add(1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer s.cur.Add(-1)
	if s.block != nil {
		<-s.block
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPaths[path] {
		return fmt.Errorf("bunny delete returned status 400 for %s", path)
	}
	s.deleted = append(s.deleted, path)
	return nil
}

func (s *fakeStorage) deletedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.deleted...)
	sort.Strings(out)
	return out
}

type trim struct {
	zpid     string
	from, to []string
}

// fakeStore is the properties table: one gallery per zpid, and a record of
// every trim.
type fakeStore struct {
	mu        sync.Mutex
	galleries map[string][]string
	loadErr   error
	trimErr   error
	trims     []trim
}

func (s *fakeStore) ImageURLs(ctx context.Context, zpid string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	urls, ok := s.galleries[zpid]
	if !ok {
		return nil, fmt.Errorf("zpid %s not stored", zpid)
	}
	return append([]string(nil), urls...), nil
}

func (s *fakeStore) TrimImageURLs(ctx context.Context, zpid string, from, to []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.trimErr != nil {
		return s.trimErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trims = append(s.trims, trim{zpid, from, to})
	if s.galleries != nil {
		s.galleries[zpid] = to
	}
	return nil
}

func gallery(zpid string, n int) []string {
	urls := make([]string, n)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/properties/%s/%d.jpg", cdn, zpid, i)
	}
	return urls
}

func newPurger(storage *fakeStorage, store *fakeStore, concurrency int) *Purger {
	return New(storage, store, concurrency, slog.New(slog.DiscardHandler))
}

func TestPurgeURLs_KeepsTheFirstPhotoAndDeletesTheRest(t *testing.T) {
	storage := &fakeStorage{}
	store := &fakeStore{}
	urls := gallery("42", 4)

	n, err := newPurger(storage, store, 4).PurgeURLs(context.Background(), "42", urls)
	if err != nil {
		t.Fatalf("PurgeURLs: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}
	want := []string{"properties/42/1.jpg", "properties/42/2.jpg", "properties/42/3.jpg"}
	if got := storage.deletedPaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("deleted paths = %v, want %v", got, want)
	}
	wantTrims := []trim{{"42", urls, urls[:1]}}
	if !reflect.DeepEqual(store.trims, wantTrims) {
		t.Errorf("trims = %+v, want %+v", store.trims, wantTrims)
	}
}

// The row is trimmed only after every object is gone. Otherwise a listing
// would point at nothing while the objects it no longer lists still cost
// money, with no row left to find them by.
func TestPurgeURLs_ADeleteFailureLeavesTheRowUntrimmed(t *testing.T) {
	storage := &fakeStorage{failPaths: map[string]bool{"properties/42/2.jpg": true}}
	store := &fakeStore{}

	_, err := newPurger(storage, store, 1).PurgeURLs(context.Background(), "42", gallery("42", 4))
	if err == nil || !strings.Contains(err.Error(), "2.jpg") {
		t.Fatalf("err = %v, want the failed object's error", err)
	}
	if len(store.trims) != 0 {
		t.Errorf("row trimmed to %v after a failed delete", store.trims)
	}
	// The others were still removed: the next run finds them gone (404) and
	// only has the failed one left to do.
	if got := storage.deletedPaths(); len(got) != 2 {
		t.Errorf("deleted = %v, want the two that did not fail", got)
	}
}

func TestPurgeURLs_AGalleryOfOneOrNoneIsLeftAlone(t *testing.T) {
	for _, urls := range [][]string{nil, {}, gallery("42", 1)} {
		storage := &fakeStorage{}
		store := &fakeStore{}

		n, err := newPurger(storage, store, 4).PurgeURLs(context.Background(), "42", urls)
		if err != nil || n != 0 {
			t.Errorf("PurgeURLs(%v) = %d, %v; want 0, nil", urls, n, err)
		}
		if len(storage.deletedPaths()) != 0 || len(store.trims) != 0 {
			t.Errorf("PurgeURLs(%v) deleted %v and trimmed %v; want nothing", urls, storage.deletedPaths(), store.trims)
		}
	}
}

// A gallery that is not entirely ours is not understood, so none of it is
// touched: a source URL means the row was never moved to the CDN (or was
// refreshed with new sources in between), and deleting around it would leave
// a mixed row.
func TestPurgeURLs_RefusesAGalleryWithAForeignURL(t *testing.T) {
	cases := map[string][]string{
		"source URL first":         {"https://photos.zillowstatic.com/a.jpg", cdn + "/properties/42/1.jpg", cdn + "/properties/42/2.jpg"},
		"source URL in the middle": {cdn + "/properties/42/0.jpg", "https://photos.zillowstatic.com/a.jpg", cdn + "/properties/42/2.jpg"},
		"source URL last":          {cdn + "/properties/42/0.jpg", cdn + "/properties/42/1.jpg", "https://photos.zillowstatic.com/a.jpg"},
	}
	for name, urls := range cases {
		storage := &fakeStorage{}
		store := &fakeStore{}

		_, err := newPurger(storage, store, 4).PurgeURLs(context.Background(), "42", urls)
		if err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
		if len(storage.deletedPaths()) != 0 || len(store.trims) != 0 {
			t.Errorf("%s: deleted %v and trimmed %v; want nothing", name, storage.deletedPaths(), store.trims)
		}
	}
}

// Only objects of the listing's own folder are ever deleted. The column holds
// photo URLs by convention; a video, a map or another listing's photo that
// found its way into it (a manual fix, a later feature) must not be deleted
// on the strength of that convention.
func TestPurgeURLs_RefusesAnythingOutsideTheListingsOwnFolder(t *testing.T) {
	own := func(i int) string { return fmt.Sprintf("%s/properties/42/%d.jpg", cdn, i) }
	cases := map[string][]string{
		"a video":                 {own(0), own(1), cdn + "/videos/42.mp4"},
		"a map":                   {own(0), cdn + "/maps/v1/42.png", own(2)},
		"another listing's photo": {own(0), own(1), cdn + "/properties/43/1.jpg"},
		"a nested path":           {own(0), own(1), cdn + "/properties/42/x/1.jpg"},
		"a folder prefix":         {own(0), own(1), cdn + "/properties/420/1.jpg"},
		"the folder itself":       {own(0), own(1), cdn + "/properties/42"},
	}
	for name, urls := range cases {
		storage := &fakeStorage{}
		store := &fakeStore{}

		_, err := newPurger(storage, store, 4).PurgeURLs(context.Background(), "42", urls)
		if err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
		if len(storage.deletedPaths()) != 0 || len(store.trims) != 0 {
			t.Errorf("%s: deleted %v and trimmed %v; want nothing", name, storage.deletedPaths(), store.trims)
		}
	}
}

// Check is the same refusal without the deletes, for a dry run that wants
// to report what a real run would actually do.
func TestCheck(t *testing.T) {
	p := newPurger(&fakeStorage{}, &fakeStore{}, 4)
	if err := p.Check("42", gallery("42", 3)); err != nil {
		t.Errorf("Check of a good gallery: %v", err)
	}
	if err := p.Check("42", []string{cdn + "/properties/42/0.jpg", cdn + "/videos/42.mp4"}); err == nil {
		t.Error("Check of a gallery with a video = nil, want a refusal")
	}
	if err := p.Check("42", []string{"https://photos.zillowstatic.com/a.jpg", cdn + "/properties/42/1.jpg"}); err == nil {
		t.Error("Check of a gallery with a source URL = nil, want a refusal")
	}
}

func TestPurgeURLs_ATrimFailureIsReturned(t *testing.T) {
	storage := &fakeStorage{}
	store := &fakeStore{trimErr: errors.New("image_urls changed underneath")}

	_, err := newPurger(storage, store, 4).PurgeURLs(context.Background(), "42", gallery("42", 3))
	if err == nil || !strings.Contains(err.Error(), "changed underneath") {
		t.Fatalf("err = %v, want the store's error", err)
	}
}

func TestPurgeURLs_DeletesAtMostConcurrencyAtOnce(t *testing.T) {
	storage := &fakeStorage{block: make(chan struct{})}
	store := &fakeStore{}
	done := make(chan error, 1)
	go func() {
		_, err := newPurger(storage, store, 3).PurgeURLs(context.Background(), "42", gallery("42", 20))
		done <- err
	}()
	// Let the goroutines pile up against the block, then release them.
	for storage.cur.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	close(storage.block)
	if err := <-done; err != nil {
		t.Fatalf("PurgeURLs: %v", err)
	}
	if peak := storage.peak.Load(); peak != 3 {
		t.Errorf("peak concurrent deletes = %d, want 3", peak)
	}
	if n := len(storage.deletedPaths()); n != 19 {
		t.Errorf("deleted %d, want 19", n)
	}
}

func TestPurgeURLs_StopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := &fakeStorage{}
	store := &fakeStore{}

	_, err := newPurger(storage, store, 4).PurgeURLs(ctx, "42", gallery("42", 5))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(store.trims) != 0 {
		t.Errorf("row trimmed to %v although nothing was deleted", store.trims)
	}
}

// Purge reads the gallery from the row itself. The worker's property has the
// provider's source URLs on a revisit, and on a fresh listing the CDN URLs it
// just stored — either way the row is what the public reads, so it is what is
// purged.
func TestPurge_ReadsTheGalleryFromTheRow(t *testing.T) {
	storage := &fakeStorage{}
	store := &fakeStore{galleries: map[string][]string{"42": gallery("42", 3)}}

	n, err := newPurger(storage, store, 4).Purge(context.Background(), "42")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}
	if got := store.galleries["42"]; !reflect.DeepEqual(got, gallery("42", 1)) {
		t.Errorf("row now %v, want the first photo only", got)
	}
}

func TestPurge_ALoadFailureIsReturned(t *testing.T) {
	storage := &fakeStorage{}
	store := &fakeStore{loadErr: errors.New("db down")}

	_, err := newPurger(storage, store, 4).Purge(context.Background(), "42")
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the store's error", err)
	}
	if len(storage.deletedPaths()) != 0 {
		t.Errorf("deleted %v without knowing the gallery", storage.deletedPaths())
	}
}
