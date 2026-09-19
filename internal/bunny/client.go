// Package bunny uploads files to Bunny CDN Storage and returns public CDN URLs.
package bunny

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxErrorBody bounds how much of an error response is quoted in an error.
const maxErrorBody = 512

// maxDrain bounds how much of a response is read before it is closed. Bunny
// answers a PUT with a few dozen bytes of JSON; reading them to EOF is what
// lets an HTTP/1.1 connection go back into the pool instead of paying for a
// TLS handshake per object.
const maxDrain = 4 << 10

// errAttemptOver is what a transport goroutine gets when it reads an attempt's
// body after that attempt has ended. See attemptBody.
var errAttemptOver = errors.New("bunny: upload attempt is over")

// Client uploads objects to a Bunny CDN storage zone.
type Client struct {
	storageZone string
	apiKey      string
	cdnBaseURL  string
	http        *http.Client

	// endpointBase is scheme + host of the storage API. New always builds it
	// as "https://" + storageHost; it is a field only so tests can aim the
	// client at an httptest server.
	endpointBase string
	// backoff is the pause before each retry, so a rewindable body gets
	// len(backoff)+1 attempts. A field so tests do not sleep.
	backoff []time.Duration
}

// New creates a Bunny CDN storage client.
//   - storageHost: e.g. "storage.bunnycdn.com" or a regional host like "la.storage.bunnycdn.com"
//   - cdnBaseURL:  the public pull-zone base, e.g. "https://dwellings.b-cdn.net"
func New(storageZone, apiKey, storageHost, cdnBaseURL string, timeout time.Duration) *Client {
	return &Client{
		storageZone: storageZone,
		apiKey:      apiKey,
		cdnBaseURL:  strings.TrimRight(cdnBaseURL, "/"),
		http:        &http.Client{Timeout: timeout},

		endpointBase: "https://" + strings.TrimRight(storageHost, "/"),
		// Three attempts, 1 s then 4 s apart: long enough to ride out a
		// storage node restarting or a burst of 429s, short enough to stay
		// far inside the lease of the claim the upload runs under.
		backoff: []time.Duration{1 * time.Second, 4 * time.Second},
	}
}

// Upload streams the given content to path within the storage zone and returns
// the public CDN URL. path should not start with a slash, e.g.
// "properties/12345/0.jpg".
//
// A network error, a 429 or a 5xx is retried, but only when content is an
// io.Seeker: a retry has to send the body again from its first byte. Such a
// body is rewound before every attempt, the first included, so it is always
// uploaded from its start whatever offset the caller left it at. Any other
// reader gets the single attempt it always had, and any other status fails at
// once — Bunny rejecting the request will reject it again.
//
// Upload never closes seekable content — the caller opened it and closes it —
// and once Upload has returned nothing reads from it any more. (A reader that
// cannot seek but can be closed is still closed by net/http, as it always was.)
func (c *Client) Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error) {
	path = strings.TrimLeft(path, "/")
	endpoint := fmt.Sprintf("%s/%s/%s", c.endpointBase, c.storageZone, path)
	cdnURL := fmt.Sprintf("%s/%s", c.cdnBaseURL, path)

	seeker, size, ok := rewindable(content)
	if !ok {
		if _, err := c.put(ctx, endpoint, content, -1, contentType); err != nil {
			return "", err
		}
		return cdnURL, nil
	}

	attempts := len(c.backoff) + 1
	for attempt := 1; ; attempt++ {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("bunny upload: rewind body for attempt %d of %d: %w", attempt, attempts, err)
		}
		body := &attemptBody{r: content}
		retryable, err := c.put(ctx, endpoint, body, size, contentType)
		body.detach()
		if err == nil {
			return cdnURL, nil
		}
		if !retryable {
			return "", err
		}
		if attempt == attempts {
			return "", fmt.Errorf("gave up after %d attempts: %w", attempts, err)
		}
		if waitErr := wait(ctx, c.backoff[attempt-1]); waitErr != nil {
			return "", fmt.Errorf("bunny upload abandoned after attempt %d of %d (%v): %w", attempt, attempts, err, waitErr)
		}
	}
}

// put makes one PUT attempt. retryable reports whether the failure is one that
// sending the same bytes again could cure: the network, Bunny shedding load
// (429) or Bunny failing (5xx). size is the length of body, sent as the
// Content-Length, or -1 when only reading it will tell.
func (c *Client) put(ctx context.Context, endpoint string, body io.Reader, size int64, contentType string) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return false, fmt.Errorf("build upload request: %w", err)
	}
	switch {
	case size > 0:
		// NewRequest only knows the length of a few in-memory readers. Without
		// it a file goes out chunked, and with it the transport refuses to
		// send a body that turns out shorter or longer than announced.
		req.ContentLength = size
	case size == 0:
		// A ContentLength of 0 next to a body means "unknown" to net/http,
		// which would send an empty chunked body; NoBody is how to say zero.
		req.Body = http.NoBody
	}
	req.Header.Set("AccessKey", c.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("bunny upload: %w", err)
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(res.Body, maxDrain))

	if res.StatusCode == http.StatusCreated || res.StatusCode == http.StatusOK {
		return false, nil
	}
	if len(answer) > maxErrorBody {
		answer = answer[:maxErrorBody]
	}
	retryable = res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= http.StatusInternalServerError
	return retryable, fmt.Errorf("bunny upload returned status %d: %s", res.StatusCode, strings.TrimSpace(string(answer)))
}

// rewindable reports whether content can be sent more than once and, if so,
// how long it is. A Seek method alone does not prove it — an *os.File that is
// a pipe has one that always fails — so the length probe doubles as the test;
// a reader that fails it has not been moved and is streamed once, as-is.
func rewindable(content io.Reader) (io.Seeker, int64, bool) {
	seeker, ok := content.(io.Seeker)
	if !ok {
		return nil, 0, false
	}
	size, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, false
	}
	return seeker, size, true
}

// wait pauses for d, or until ctx is done, and reports ctx's error: a worker
// that is shutting down or past its deadline must not sit out a backoff.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	return ctx.Err()
}

// attemptBody lends the caller's reader to exactly one HTTP attempt. It is an
// io.NopCloser that can also be taken back.
//
// Close does nothing, where handing net/http the *os.File itself would let the
// transport close it after the first attempt: the second could not rewind it,
// and it is the caller's to close in any case.
//
// detach ends the loan. net/http reads a request body on a goroutine of its
// own, which can still be reading after Do has returned — typically when
// Bunny answers 503 before it has consumed a large video. Rewinding the file
// for the next attempt under that goroutine would let it steal bytes from the
// retry, and a truncated object could be stored under a 201. detach waits out
// a Read in progress and fails every later one without touching the reader, so
// the retry — and the caller, once Upload returns — has the reader to itself.
type attemptBody struct {
	mu   sync.Mutex
	r    io.Reader
	over bool
}

func (b *attemptBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over {
		return 0, errAttemptOver
	}
	return b.r.Read(p)
}

func (b *attemptBody) Close() error { return nil }

func (b *attemptBody) detach() {
	b.mu.Lock()
	b.over = true
	b.mu.Unlock()
}
