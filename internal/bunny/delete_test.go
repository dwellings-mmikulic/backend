package bunny

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDelete_SendsADeleteForTheObject(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusOK}}
	c := newTestClient(t, srv)

	if err := c.Delete(context.Background(), "/properties/42/3.jpg"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", r.method)
	}
	if want := "/" + testZone + "/properties/42/3.jpg"; r.path != want {
		t.Errorf("path = %q, want %q", r.path, want)
	}
	if r.accessKey != testKey {
		t.Errorf("AccessKey = %q, want %q", r.accessKey, testKey)
	}
}

// An object that is already gone is the outcome a delete wants: a purge that
// is retried after a crash must not fail on the half it already did.
func TestDelete_NotFoundIsSuccess(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusNotFound}}
	c := newTestClient(t, srv)

	if err := c.Delete(context.Background(), "properties/42/3.jpg"); err != nil {
		t.Fatalf("Delete of a missing object: %v, want nil", err)
	}
}

func TestDelete_RetriedAfter500And429(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusOK}}
	c := newTestClient(t, srv)

	if err := c.Delete(context.Background(), "properties/42/3.jpg"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := len(srv.requests()); n != 3 {
		t.Errorf("server saw %d requests, want 3", n)
	}
}

func TestDelete_GivesUpAfterThreeAttempts(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusServiceUnavailable}}
	c := newTestClient(t, srv)

	err := c.Delete(context.Background(), "properties/42/3.jpg")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want a 503 error", err)
	}
	if n := len(srv.requests()); n != 3 {
		t.Errorf("server saw %d requests, want 3", n)
	}
}

// Bunny answers a refused delete with 400. That is not cured by sending it
// again, and it must surface: a purge that trimmed the row on it would leave
// an object nobody can find any more.
func TestDelete_BadRequestIsAnErrorWithoutRetry(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusBadRequest}}
	c := newTestClient(t, srv)

	err := c.Delete(context.Background(), "properties/42/3.jpg")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want a 400 error", err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
}

// Bunny deletes a directory recursively, and the zone root is a directory.
func TestDelete_RefusesTheRootAndDirectories(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusOK}}
	c := newTestClient(t, srv)

	for _, p := range []string{"", "/", "//", "properties/", "properties/42/", "/properties/42/"} {
		if err := c.Delete(context.Background(), p); err == nil {
			t.Errorf("Delete(%q) = nil, want an error", p)
		}
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("server saw %d requests, want none", n)
	}
}

func TestDelete_StopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := &storageServer{statuses: []int{http.StatusOK}}
	c := newTestClient(t, srv)

	if err := c.Delete(ctx, "properties/42/3.jpg"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestObjectPath(t *testing.T) {
	c := New(testZone, testKey, "storage.invalid", testCDN+"/", time.Second)

	good := map[string]string{
		testCDN + "/properties/42/3.jpg": "properties/42/3.jpg",
		testCDN + "/videos/42.mp4":       "videos/42.mp4",
		testCDN + "/hls/v1/42/ab/seg.ts": "hls/v1/42/ab/seg.ts",
		testCDN + "/properties/42/0.png": "properties/42/0.png",
	}
	for url, want := range good {
		got, err := c.ObjectPath(url)
		if err != nil || got != want {
			t.Errorf("ObjectPath(%q) = %q, %v; want %q", url, got, err, want)
		}
	}

	// Anything that is not one object of this zone is refused: a source URL
	// (Zillow) left in a row, another host, the CDN root, a directory, a
	// traversal, a query string.
	bad := []string{
		"https://photos.zillowstatic.com/fp/abc.jpg",
		"https://other.b-cdn.net/properties/42/3.jpg",
		testCDN,
		testCDN + "/",
		testCDN + "//",
		testCDN + "/properties/42/",
		testCDN + "/properties/../videos/42.mp4",
		testCDN + "/properties/42/3.jpg?x=1",
		testCDN + "/properties/42/3.jpg#f",
		"",
	}
	for _, url := range bad {
		if got, err := c.ObjectPath(url); err == nil {
			t.Errorf("ObjectPath(%q) = %q, nil; want an error", url, got)
		}
	}
}
