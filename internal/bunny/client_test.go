package bunny

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testZone = "test-zone"
	testKey  = "test-access-key"
	testCDN  = "https://cdn.example"
)

// seenRequest is what the fake storage server recorded about one PUT.
type seenRequest struct {
	method           string
	path             string
	accessKey        string
	contentType      string
	contentLength    int64
	transferEncoding []string
	body             []byte
}

// storageServer is a fake Bunny storage endpoint: it records every request in
// full and answers attempt N with statuses[N-1] (the last status repeats).
type storageServer struct {
	mu       sync.Mutex
	statuses []int
	seen     []seenRequest
}

func (s *storageServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.seen = append(s.seen, seenRequest{
		method:           r.Method,
		path:             r.URL.Path,
		accessKey:        r.Header.Get("AccessKey"),
		contentType:      r.Header.Get("Content-Type"),
		contentLength:    r.ContentLength,
		transferEncoding: r.TransferEncoding,
		body:             body,
	})
	status := s.statuses[min(len(s.seen), len(s.statuses))-1]
	s.mu.Unlock()

	w.WriteHeader(status)
	_, _ = io.WriteString(w, http.StatusText(status))
}

func (s *storageServer) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

// newTestClient returns a production-built client pointed at a fake storage
// server, with the retry pauses removed so no test sleeps.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := New(testZone, testKey, "storage.invalid", testCDN+"/", 10*time.Second)
	c.endpointBase = srv.URL
	c.backoff = []time.Duration{0, 0}
	return c
}

// testPayload is deterministic, non-repeating-enough content: a body that was
// sent from the wrong offset, truncated or interleaved cannot compare equal.
func testPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ byte(i>>8) ^ byte(i>>16)
	}
	return b
}

// tempFile writes content to a real file and returns it open for reading, the
// way the scheduler hands a rendered MP4 to Upload.
func tempFile(t *testing.T, content []byte) *os.File {
	t.Helper()
	name := filepath.Join(t.TempDir(), "upload.bin")
	if err := os.WriteFile(name, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestNew_ProductionDefaults(t *testing.T) {
	c := New("zone", "key", "la.storage.bunnycdn.com/", "https://dwellings.b-cdn.net/", time.Minute)

	if c.endpointBase != "https://la.storage.bunnycdn.com" {
		t.Errorf("endpointBase = %q, want https:// + the storage host", c.endpointBase)
	}
	if len(c.backoff) != 2 || c.backoff[0] != time.Second || c.backoff[1] != 4*time.Second {
		t.Errorf("backoff = %v, want [1s 4s] (three attempts)", c.backoff)
	}
	if c.http.Timeout != time.Minute {
		t.Errorf("http timeout = %v, want 1m", c.http.Timeout)
	}
}

// The case the retry exists for, with the body type production uses. Without
// shielding the file from the transport's Close, attempt two would fail to
// rewind a closed file.
func TestUpload_FileRetriedAfter500(t *testing.T) {
	payload := testPayload(300 << 10)
	f := tempFile(t, payload)
	srv := &storageServer{statuses: []int{http.StatusInternalServerError, http.StatusCreated}}
	c := newTestClient(t, srv)

	url, err := c.Upload(context.Background(), "videos/42.mp4", f, "video/mp4")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if want := testCDN + "/videos/42.mp4"; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(reqs))
	}
	for i, r := range reqs {
		if !bytes.Equal(r.body, payload) {
			t.Errorf("attempt %d: server got %d body bytes that differ from the %d-byte file", i+1, len(r.body), len(payload))
		}
		if r.contentLength != int64(len(payload)) || len(r.transferEncoding) != 0 {
			t.Errorf("attempt %d: Content-Length = %d, Transfer-Encoding = %v; want %d and none",
				i+1, r.contentLength, r.transferEncoding, len(payload))
		}
		if r.method != http.MethodPut || r.path != "/"+testZone+"/videos/42.mp4" {
			t.Errorf("attempt %d: %s %s, want PUT /%s/videos/42.mp4", i+1, r.method, r.path, testZone)
		}
	}

	// The file is the caller's: still open, still readable from the start.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("file unusable after Upload: %v", err)
	}
	again, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read file after Upload: %v", err)
	}
	if !bytes.Equal(again, payload) {
		t.Errorf("file re-read after Upload gave %d bytes that differ from the original %d", len(again), len(payload))
	}
}

func TestUpload_RetriedAfter429(t *testing.T) {
	payload := testPayload(4 << 10)
	srv := &storageServer{statuses: []int{http.StatusTooManyRequests, http.StatusOK}}
	c := newTestClient(t, srv)

	if _, err := c.Upload(context.Background(), "a/b.jpg", bytes.NewReader(payload), "image/jpeg"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(reqs))
	}
	if !bytes.Equal(reqs[1].body, payload) {
		t.Errorf("retry body differs from the payload (%d vs %d bytes)", len(reqs[1].body), len(payload))
	}
}

func TestUpload_GivesUpAfterThreeAttempts(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusServiceUnavailable}}
	c := newTestClient(t, srv)

	url, err := c.Upload(context.Background(), "a/b.jpg", bytes.NewReader(testPayload(1024)), "image/jpeg")
	if err == nil {
		t.Fatalf("Upload succeeded with url %q against a server that only answers 503", url)
	}
	if url != "" {
		t.Errorf("url = %q on failure, want empty", url)
	}
	if n := len(srv.requests()); n != 3 {
		t.Errorf("server saw %d requests, want 3", n)
	}
	for _, want := range []string{"3 attempts", "503", http.StatusText(http.StatusServiceUnavailable)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A 4xx is Bunny rejecting this request (bad key, bad path): sending it again
// cannot help and would only delay the failure.
func TestUpload_ClientErrorIsNotRetried(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusUnauthorized}}
	c := newTestClient(t, srv)

	_, err := c.Upload(context.Background(), "a/b.jpg", bytes.NewReader(testPayload(1024)), "image/jpeg")
	if err == nil {
		t.Fatal("Upload succeeded against a 401")
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1", n)
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), http.StatusText(http.StatusUnauthorized)) {
		t.Errorf("error %q does not carry the status and body", err)
	}
}

// What Bunny — or a proxy in front of it — answers is quoted in the error, and
// the error ends up in the logs and in listing_queue.last_error. An HTML error
// page must not: the quote is the head of the answer, and giving up after
// three attempts reports the last answer rather than all three.
func TestUpload_ErrorQuotesOnlyTheHeadOfTheAnswer(t *testing.T) {
	const head = "upstream says no: "
	page := head + strings.Repeat("x", 10<<10)

	for _, tc := range []struct {
		name     string
		status   int
		code     string
		requests int
	}{
		{"rejected", http.StatusUnauthorized, "401", 1},
		{"gave up", http.StatusServiceUnavailable, "503", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				requests++
				mu.Unlock()
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, page)
			}))

			_, err := c.Upload(context.Background(), "a/b.jpg", bytes.NewReader(testPayload(1024)), "image/jpeg")
			if err == nil {
				t.Fatalf("Upload succeeded against a %d", tc.status)
			}
			mu.Lock()
			got := requests
			mu.Unlock()
			if got != tc.requests {
				t.Errorf("server saw %d requests, want %d", got, tc.requests)
			}

			msg := err.Error()
			if len(msg) > 700 {
				t.Errorf("error is %d bytes for a %d-byte answer, want the 512-byte head of it and little else", len(msg), len(page))
			}
			for _, want := range []string{tc.code, head} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %.80q… does not mention %q", msg, want)
				}
			}
		})
	}
}

// countingBody is a response body that knows how much of it was read and
// whether it was closed.
type countingBody struct {
	r      io.Reader
	read   int64
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *countingBody) Close() error {
	b.closed = true
	return nil
}

// The answer is read so that the connection can go back into the pool, but an
// answer that does not end after Bunny's few dozen bytes is not followed to
// its end: neither a stored object nor a failure is worth a megabyte of it.
func TestUpload_ReadsABoundedPartOfTheAnswer(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			answer := &countingBody{r: bytes.NewReader(make([]byte, 1<<20))}
			c := New(testZone, testKey, "storage.invalid", testCDN, 10*time.Second)
			c.backoff = nil // a single attempt
			c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, req.Body)
				res := cannedResponse(req, status)
				res.Body = answer
				return res, nil
			})

			_, err := c.Upload(context.Background(), "a.bin", bytes.NewReader(testPayload(1024)), "")
			if stored := err == nil; stored != (status == http.StatusCreated) {
				t.Fatalf("Upload against a %d: err = %v", status, err)
			}
			if answer.read == 0 || answer.read > 4<<10 {
				t.Errorf("read %d bytes of a %d-byte answer, want some of it and at most 4 KiB", answer.read, 1<<20)
			}
			if !answer.closed {
				t.Error("the response body was not closed")
			}
		})
	}
}

// A body that cannot be rewound cannot be sent twice.
func TestUpload_NonSeekableBodyGetsOneAttempt(t *testing.T) {
	payload := testPayload(64 << 10)
	srv := &storageServer{statuses: []int{http.StatusInternalServerError}}
	c := newTestClient(t, srv)

	content := io.MultiReader(bytes.NewReader(payload[:1000]), bytes.NewReader(payload[1000:]))
	_, err := c.Upload(context.Background(), "a/b.bin", content, "application/octet-stream")
	if err == nil {
		t.Fatal("Upload succeeded against a 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not carry the status", err)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want exactly 1", len(reqs))
	}
	if !bytes.Equal(reqs[0].body, payload) {
		t.Errorf("server got %d body bytes that differ from the %d-byte stream", len(reqs[0].body), len(payload))
	}
}

func TestUpload_NonSeekableBodySucceeds(t *testing.T) {
	payload := testPayload(64 << 10)
	srv := &storageServer{statuses: []int{http.StatusCreated}}
	c := newTestClient(t, srv)

	pr, pw := io.Pipe()
	go func() {
		_, err := pw.Write(payload)
		_ = pw.CloseWithError(err)
	}()
	url, err := c.Upload(context.Background(), "a/b.bin", pr, "")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != testCDN+"/a/b.bin" {
		t.Errorf("url = %q", url)
	}
	reqs := srv.requests()
	if len(reqs) != 1 || !bytes.Equal(reqs[0].body, payload) {
		t.Fatalf("server saw %d requests / a body that differs from the stream", len(reqs))
	}
	if reqs[0].contentType != "" {
		t.Errorf("Content-Type = %q, want none when the caller passes none", reqs[0].contentType)
	}
}

func TestUpload_HeadersOnEveryAttempt(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusBadGateway, http.StatusTooManyRequests, http.StatusCreated}}
	c := newTestClient(t, srv)

	if _, err := c.Upload(context.Background(), "hls/v1/1/ab/seg-000.ts", bytes.NewReader(testPayload(2048)), "video/MP2T"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(reqs))
	}
	for i, r := range reqs {
		if r.accessKey != testKey {
			t.Errorf("attempt %d: AccessKey = %q, want %q", i+1, r.accessKey, testKey)
		}
		if r.contentType != "video/MP2T" {
			t.Errorf("attempt %d: Content-Type = %q, want video/MP2T", i+1, r.contentType)
		}
		if r.contentLength != 2048 {
			t.Errorf("attempt %d: Content-Length = %d, want 2048", i+1, r.contentLength)
		}
	}
}

func TestUpload_TrimsLeadingSlashFromPath(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusCreated}}
	c := newTestClient(t, srv) // built with a trailing slash on the CDN base

	url, err := c.Upload(context.Background(), "//properties/12345/0.jpg", bytes.NewReader([]byte("jpeg")), "image/jpeg")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if want := testCDN + "/properties/12345/0.jpg"; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
	if got, want := srv.requests()[0].path, "/"+testZone+"/properties/12345/0.jpg"; got != want {
		t.Errorf("request path = %q, want %q", got, want)
	}
}

// A seekable body is sent from its first byte even when the caller has
// already read from it: a retry must rewind, so every attempt does.
func TestUpload_SeekableBodyIsSentFromItsStart(t *testing.T) {
	payload := testPayload(8 << 10)
	srv := &storageServer{statuses: []int{http.StatusCreated}}
	c := newTestClient(t, srv)

	r := bytes.NewReader(payload)
	if _, err := io.CopyN(io.Discard, r, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(context.Background(), "a.bin", r, ""); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := srv.requests()[0].body; !bytes.Equal(got, payload) {
		t.Errorf("server got %d bytes, want the whole %d-byte payload", len(got), len(payload))
	}
}

func TestUpload_EmptySeekableBody(t *testing.T) {
	srv := &storageServer{statuses: []int{http.StatusCreated}}
	c := newTestClient(t, srv)

	if _, err := c.Upload(context.Background(), "empty.bin", bytes.NewReader(nil), ""); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if r := reqs[0]; len(r.body) != 0 || r.contentLength != 0 || len(r.transferEncoding) != 0 {
		t.Errorf("body %d bytes, Content-Length %d, Transfer-Encoding %v; want an empty body announced as Content-Length: 0",
			len(r.body), r.contentLength, r.transferEncoding)
	}
}

// An *os.File that is a pipe has a Seek method that always fails. It is a
// stream, so it gets the stream's single attempt rather than an error.
func TestUpload_UnseekableFileGetsOneAttempt(t *testing.T) {
	payload := testPayload(16 << 10)
	srv := &storageServer{statuses: []int{http.StatusInternalServerError}}
	c := newTestClient(t, srv)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	go func() {
		_, _ = pw.Write(payload)
		_ = pw.Close()
	}()

	if _, err := c.Upload(context.Background(), "a.bin", pr, ""); err == nil {
		t.Fatal("Upload succeeded against a 500")
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want exactly 1", len(reqs))
	}
	if !bytes.Equal(reqs[0].body, payload) {
		t.Errorf("server got %d body bytes that differ from the %d-byte stream", len(reqs[0].body), len(payload))
	}
}

// A connection that dies is the other retryable failure. The first attempt's
// connection is hijacked and dropped without an answer.
func TestUpload_RetriedAfterTransportError(t *testing.T) {
	payload := testPayload(32 << 10)
	ok := &storageServer{statuses: []int{http.StatusCreated}}
	var mu sync.Mutex
	dropped := 0
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := dropped == 0
		if first {
			dropped++
		}
		mu.Unlock()
		if !first {
			ok.ServeHTTP(w, r)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))

	if _, err := c.Upload(context.Background(), "a.bin", bytes.NewReader(payload), ""); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	reqs := ok.requests()
	if len(reqs) != 1 || !bytes.Equal(reqs[0].body, payload) {
		t.Fatalf("after the dropped connection the server saw %d complete requests / a wrong body", len(reqs))
	}
}

// The straggler scenario below, but against the real transport: the first
// answer arrives while net/http is still busy writing an 8 MiB body that the
// server never reads. How that attempt ends (a 503, or a broken connection if
// the kernel resets first) is up to the network stack; either way the retry
// must deliver the whole file, and ending the attempt must not deadlock with a
// transport goroutine parked in a socket write.
func TestUpload_AnswerBeforeBodyIsConsumed(t *testing.T) {
	payload := testPayload(8 << 20)
	f := tempFile(t, payload)

	ok := &storageServer{statuses: []int{http.StatusCreated}}
	var mu sync.Mutex
	early := 0
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := early == 0
		if first {
			early++
		}
		mu.Unlock()
		if !first {
			ok.ServeHTTP(w, r)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Left open until the test ends, so the client sees the answer
		// rather than a reset.
		t.Cleanup(func() { _ = conn.Close() })
		_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}))

	if _, err := c.Upload(context.Background(), "videos/big.mp4", f, "video/mp4"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	reqs := ok.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d complete requests after the early 503, want 1", len(reqs))
	}
	if !bytes.Equal(reqs[0].body, payload) {
		t.Errorf("retry delivered %d bytes that differ from the %d-byte file", len(reqs[0].body), len(payload))
	}
}

// Real transport, real clock: the context is cancelled shortly after the
// first 500 has been served, while the (hour-long) backoff is pending.
func TestUpload_CancelledContextStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &storageServer{statuses: []int{http.StatusInternalServerError}}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
		time.AfterFunc(20*time.Millisecond, cancel)
	}))
	c.backoff = []time.Duration{time.Hour, time.Hour}

	start := time.Now()
	_, err := c.Upload(ctx, "a.bin", bytes.NewReader(testPayload(1024)), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Upload took %v to notice the cancelled context", elapsed)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1", n)
	}
}

// roundTripFunc lets a test stand in for the HTTP transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cannedResponse(req *http.Request, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(http.StatusText(status))),
		Request:    req,
	}
}

// Deterministic version of the above: the context is already cancelled when
// the 503 comes back, so the only way out is the backoff wait giving up.
func TestUpload_BackoffIsAbandonedWhenContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempts := 0
	c := New(testZone, testKey, "storage.invalid", testCDN, 10*time.Second)
	c.backoff = []time.Duration{time.Hour, time.Hour}
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		_, _ = io.Copy(io.Discard, req.Body)
		cancel()
		return cannedResponse(req, http.StatusServiceUnavailable), nil
	})

	_, err := c.Upload(ctx, "a.bin", bytes.NewReader(testPayload(1024)), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q does not say what the last attempt failed with", err)
	}
	if attempts != 1 {
		t.Errorf("transport saw %d attempts, want 1", attempts)
	}
}

// The pause before retry k is backoff[k-1]: New's {1 s, 4 s} has to mean 1 s
// and then 4 s, not 1 s twice. Only lower bounds are asserted — a timer never
// fires early, so a loaded machine cannot fail this — and each order catches
// the schedule collapsing onto one entry: onto the first when the long pause
// comes last, onto the last when it comes first.
func TestUpload_PausesFollowTheBackoffSchedule(t *testing.T) {
	const short, long = 10 * time.Millisecond, 120 * time.Millisecond
	for _, tc := range []struct {
		name    string
		backoff []time.Duration
	}{
		{"long pause last", []time.Duration{short, long}},
		{"long pause first", []time.Duration{long, short}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var at []time.Time
			c := New(testZone, testKey, "storage.invalid", testCDN, 10*time.Second)
			c.backoff = tc.backoff
			c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				at = append(at, time.Now())
				_, _ = io.Copy(io.Discard, req.Body)
				return cannedResponse(req, http.StatusServiceUnavailable), nil
			})

			if _, err := c.Upload(context.Background(), "a.bin", bytes.NewReader(testPayload(1024)), ""); err == nil {
				t.Fatal("Upload succeeded against a transport that only answers 503")
			}
			if len(at) != len(tc.backoff)+1 {
				t.Fatalf("transport saw %d attempts, want %d", len(at), len(tc.backoff)+1)
			}
			for i, want := range tc.backoff {
				if got := at[i+1].Sub(at[i]); got < want {
					t.Errorf("pause before attempt %d = %v, want at least backoff[%d] = %v", i+2, got, i, want)
				}
			}
		})
	}
}

// net/http reads the request body on a goroutine of its own and hands back an
// early response (Bunny answering 503 before it has consumed a large video)
// while that goroutine is still reading. If it could keep reading the caller's
// file after the attempt is over, it would steal bytes from the retry and a
// corrupt object would be stored under a 201. This transport makes that
// straggler deterministic: it is released only once attempt two has begun and
// attempt two reads only after the straggler has done its worst.
func TestUpload_StaleTransportReaderCannotStealFromRetry(t *testing.T) {
	payload := testPayload(300 << 10)
	f := tempFile(t, payload)

	var (
		attempts  int
		release   = make(chan struct{})
		straggler = make(chan struct{})
		stolen    int64
		stolenErr error
		got       []byte
	)
	c := New(testZone, testKey, "storage.invalid", testCDN, 10*time.Second)
	c.backoff = []time.Duration{0, 0}
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			if _, err := io.ReadFull(req.Body, make([]byte, 4096)); err != nil {
				t.Errorf("attempt 1: read first chunk: %v", err)
			}
			go func() {
				defer close(straggler)
				<-release
				stolen, stolenErr = io.Copy(io.Discard, req.Body)
			}()
			return cannedResponse(req, http.StatusServiceUnavailable), nil
		}
		close(release)
		<-straggler
		got, _ = io.ReadAll(req.Body)
		return cannedResponse(req, http.StatusCreated), nil
	})

	if _, err := c.Upload(context.Background(), "videos/42.mp4", f, "video/mp4"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("transport saw %d attempts, want 2", attempts)
	}
	if stolen != 0 || stolenErr == nil {
		t.Errorf("stale reader of attempt 1 read %d bytes (err %v) after the attempt was over; want 0 bytes and an error", stolen, stolenErr)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("attempt 2 sent %d bytes that differ from the %d-byte file", len(got), len(payload))
	}
}

// Same guarantee towards the caller: once Upload has returned — with an error
// or with the URL — a transport goroutine left over from the last attempt no
// longer touches the file. A success leaves one behind too: having written
// Content-Length bytes, net/http reads the body once more to check that it
// really ends there, on its write goroutine, which can run after Do returned.
//
// The caller here does what it is entitled to and rewinds its file. A read
// through the finished attempt must then fail without moving it; asserting
// only "0 bytes and an error" would be satisfied by the io.EOF of a file the
// transport had read to its end.
func TestUpload_NothingReadsTheBodyAfterReturn(t *testing.T) {
	payload := testPayload(64 << 10)
	for _, tc := range []struct {
		name   string
		status int
		stored bool
	}{
		{"rejected before the body was read", http.StatusUnauthorized, false},
		{"stored", http.StatusCreated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tempFile(t, payload)

			var body io.ReadCloser
			c := New(testZone, testKey, "storage.invalid", testCDN, 10*time.Second)
			c.backoff = nil // a single attempt
			c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body = req.Body
				if tc.stored {
					sent, err := io.ReadAll(req.Body)
					if err != nil || !bytes.Equal(sent, payload) {
						t.Errorf("transport read %d bytes (err %v) that differ from the %d-byte file", len(sent), err, len(payload))
					}
				}
				return cannedResponse(req, tc.status), nil
			})

			_, err := c.Upload(context.Background(), "a.bin", f, "")
			if stored := err == nil; stored != tc.stored {
				t.Fatalf("Upload against a %d: err = %v", tc.status, err)
			}

			if _, err := f.Seek(0, io.SeekStart); err != nil {
				t.Fatalf("file unusable after Upload: %v", err)
			}
			if n, err := body.Read(make([]byte, 16)); n != 0 || !errors.Is(err, errAttemptOver) {
				t.Errorf("Read through the finished attempt's body = %d, %v; want 0 and errAttemptOver", n, err)
			}
			if off, err := f.Seek(0, io.SeekCurrent); err != nil || off != 0 {
				t.Errorf("file offset after the stale Read = %d, %v; want 0 (nothing consumed it)", off, err)
			}
		})
	}
}

// blockingReader parks inside Read until released, like a transport goroutine
// caught mid-read when the attempt ends.
type blockingReader struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}

// Ending the loan must wait for a Read that is already inside the caller's
// reader; otherwise the rewind for the next attempt races with it.
func TestAttemptBody_DetachWaitsForReadInProgress(t *testing.T) {
	br := &blockingReader{entered: make(chan struct{}), release: make(chan struct{})}
	body := &attemptBody{r: br}

	go func() { _, _ = body.Read(make([]byte, 8)) }()
	<-br.entered

	detached := make(chan struct{})
	go func() {
		body.detach()
		close(detached)
	}()

	select {
	case <-detached:
		t.Fatal("detach returned while a Read was still inside the underlying reader")
	case <-time.After(50 * time.Millisecond):
	}
	close(br.release)
	select {
	case <-detached:
	case <-time.After(5 * time.Second):
		t.Fatal("detach did not return after the Read finished")
	}
	if err := body.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}
