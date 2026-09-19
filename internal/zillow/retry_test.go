package zillow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/config"
)

// waitRecorder stands in for Client.wait so the retry tests assert on the
// requested back-off instead of sleeping through it.
type waitRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (w *waitRecorder) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.waits = append(w.waits, d)
	w.mu.Unlock()
	return ctx.Err()
}

func (w *waitRecorder) got() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

// newTestClient is New with the back-off waits recorded instead of slept.
func newTestClient(t *testing.T, baseURL string) (*Client, *waitRecorder) {
	t.Helper()
	c := New(baseURL, "k", 5*time.Second)
	rec := &waitRecorder{}
	c.wait = rec.wait
	return c, rec
}

// budget returns a Permit that grants n requests and then denies, together
// with the number of times it was consulted.
func budget(n int32) (Permit, *atomic.Int32) {
	var asked atomic.Int32
	return func(context.Context) bool {
		return asked.Add(1) <= n
	}, &asked
}

// step is one scripted HTTP answer.
type step struct {
	code   int
	header map[string]string
	body   string
}

// scripted answers the i-th request with steps[i]; anything past the script
// gets an empty OK page, which ends a search.
func scripted(t *testing.T, steps ...step) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(calls.Add(1)) - 1
		if i >= len(steps) {
			fmt.Fprint(w, `{"status":"OK","data":[]}`)
			return
		}
		for k, v := range steps[i].header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(steps[i].code)
		fmt.Fprint(w, steps[i].body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// pageBody is a search page holding one listing, zpid "z<n>".
func pageBody(n int) string {
	return fmt.Sprintf(`{"status":"OK","data":[{"zpid":"z%d","price":100000,"streetAddress":"s","city":"c","state":"FL","zipcode":"33950"}]}`, n)
}

var criteria = config.SearchCriteria{Location: "33950"}

func TestRetry_RetryAfterIsHonoured(t *testing.T) {
	srv, calls := scripted(t,
		step{code: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "2"}, body: "Too Many Requests"},
		step{code: http.StatusOK, body: pageBody(1)},
	)
	c, rec := newTestClient(t, srv.URL)

	res, err := c.SearchPages(context.Background(), criteria, 0, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if got, want := rec.got(), []time.Duration{2 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v (the provider's Retry-After, not the default back-off)", got, want)
	}
	if len(res.Properties) != 1 || res.NextPage != 0 {
		t.Errorf("result = %d props, NextPage %d; want 1 and 0", len(res.Properties), res.NextPage)
	}
	if res.Requests != 3 || calls.Load() != 3 {
		t.Errorf("Requests=%d calls=%d, want 3 and 3 (the 429, its retry, the empty page)", res.Requests, calls.Load())
	}
}

func TestRetry_RetryAfterVariants(t *testing.T) {
	cases := map[string]struct {
		header string
		want   time.Duration
	}{
		"capped at 60s":       {"3600", 60 * time.Second},
		"exactly the cap":     {"60", 60 * time.Second},
		"surrounding spaces":  {" 7 ", 7 * time.Second},
		"http-date ignored":   {"Wed, 21 Oct 2015 07:28:00 GMT", time.Second},
		"garbage ignored":     {"soon", time.Second},
		"negative ignored":    {"-5", time.Second},
		"zero is no guidance": {"0", time.Second},
		"absent":              {"", time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hdr := map[string]string{}
			if tc.header != "" {
				hdr["Retry-After"] = tc.header
			}
			srv, _ := scripted(t,
				step{code: http.StatusServiceUnavailable, header: hdr},
				step{code: http.StatusOK, body: pageBody(1)},
			)
			c, rec := newTestClient(t, srv.URL)

			if _, err := c.SearchPages(context.Background(), criteria, 0, nil); err != nil {
				t.Fatalf("SearchPages: %v", err)
			}
			if got, want := rec.got(), []time.Duration{tc.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("waits = %v, want %v", got, want)
			}
		})
	}
}

func TestRetry_GivesUpAfterThreeAttempts(t *testing.T) {
	srv, calls := scripted(t,
		step{code: http.StatusServiceUnavailable, body: "upstream down"},
		step{code: http.StatusServiceUnavailable, body: "upstream down"},
		step{code: http.StatusServiceUnavailable, body: "upstream down"},
		step{code: http.StatusOK, body: pageBody(1)}, // must never be reached
	)
	c, rec := newTestClient(t, srv.URL)
	permit, asked := budget(100)

	res, err := c.SearchPages(context.Background(), criteria, 0, permit)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if se.Code != http.StatusServiceUnavailable || se.Body != "upstream down" {
		t.Errorf("StatusError = %+v", se)
	}
	if !IsTransient(err) {
		t.Error("three 503s must be transient")
	}
	if res.Requests != 3 || calls.Load() != 3 || asked.Load() != 3 {
		t.Errorf("Requests=%d calls=%d permit=%d, want 3 each", res.Requests, calls.Load(), asked.Load())
	}
	if got, want := rec.got(), []time.Duration{time.Second, 3 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestRetry_ClientErrorFailsAtOnce(t *testing.T) {
	srv, calls := scripted(t, step{code: http.StatusBadRequest, body: "bad location"})
	c, rec := newTestClient(t, srv.URL)

	res, err := c.SearchPages(context.Background(), criteria, 0, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("err = %v, want a *StatusError with Code 400", err)
	}
	if IsTransient(err) {
		t.Error("a 400 must not be transient")
	}
	if res.Requests != 1 || calls.Load() != 1 || len(rec.got()) != 0 {
		t.Errorf("Requests=%d calls=%d waits=%v, want one request and no wait", res.Requests, calls.Load(), rec.got())
	}
}

func TestRetry_PermitIsAskedBeforeEveryAttempt(t *testing.T) {
	srv, calls := scripted(t,
		step{code: http.StatusInternalServerError},
		step{code: http.StatusBadGateway},
		step{code: http.StatusOK, body: pageBody(1)},
	)
	c, _ := newTestClient(t, srv.URL)
	permit, asked := budget(100)

	res, err := c.SearchPages(context.Background(), criteria, 0, permit)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	// 500, 502, page 1, empty page 2: four attempts, four reservations.
	if asked.Load() != 4 || calls.Load() != 4 || res.Requests != 4 {
		t.Errorf("permit=%d calls=%d Requests=%d, want 4 each", asked.Load(), calls.Load(), res.Requests)
	}
}

// The reservation belongs right before the send, after the back-off: a
// request reserved before a 60 s Retry-After could be spent in the next
// budget window while being charged to this one.
func TestRetry_PermitIsAskedAfterTheWait(t *testing.T) {
	var mu sync.Mutex
	var events []string
	note := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		note("request")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"status":"OK","data":{"zpid":"1"}}`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", 5*time.Second)
	c.wait = func(context.Context, time.Duration) error {
		note("wait")
		return nil
	}
	permit := func(context.Context) bool {
		note("permit")
		return true
	}

	if _, _, err := c.PropertyDetails(context.Background(), "1", permit); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"permit", "request", "wait", "permit", "request"}; !reflect.DeepEqual(events, want) {
		t.Errorf("order = %v, want %v", events, want)
	}
}

// The ledger reserves through the context it is handed: it must be the
// caller's, so a cancelled claim deadline also stops the reservation instead
// of charging a request that can no longer be sent.
func TestRetry_PermitReceivesTheCallersContext(t *testing.T) {
	type ctxKey struct{}
	srv, _ := scripted(t,
		step{code: http.StatusServiceUnavailable},
		step{code: http.StatusOK, body: `{"status":"OK","data":{"zpid":"1"}}`},
	)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")

	var asked, foreign int
	permit := func(got context.Context) bool {
		asked++
		if got.Value(ctxKey{}) != "caller" {
			foreign++
		}
		return true
	}
	if _, _, err := c.PropertyDetails(ctx, "1", permit); err != nil {
		t.Fatal(err)
	}
	if asked != 2 || foreign != 0 {
		t.Errorf("permit asked %d times, %d of them without the caller's context; want 2 and 0", asked, foreign)
	}
}

func TestRetry_PermitDeniedBeforeARetrySendsNothing(t *testing.T) {
	srv, calls := scripted(t,
		step{code: http.StatusInternalServerError},
		step{code: http.StatusOK, body: pageBody(1)}, // must never be reached
	)
	c, _ := newTestClient(t, srv.URL)
	permit, asked := budget(1)

	res, err := c.SearchPages(context.Background(), criteria, 0, permit)
	if err != nil {
		t.Fatalf("a denied permit is not an error for a search, got %v", err)
	}
	if res.NextPage != 1 || res.Requests != 1 || calls.Load() != 1 || asked.Load() != 2 {
		t.Errorf("NextPage=%d Requests=%d calls=%d permit=%d, want 1, 1, 1, 2", res.NextPage, res.Requests, calls.Load(), asked.Load())
	}
}

func TestRetry_TransportErrorIsRetriedAndTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens any more: every attempt is refused
	c, rec := newTestClient(t, srv.URL)
	permit, asked := budget(100)

	res, err := c.SearchPages(context.Background(), criteria, 0, permit)
	if err == nil {
		t.Fatal("want a transport error")
	}
	if !IsTransient(err) {
		t.Errorf("a refused connection must be transient: %v", err)
	}
	if res.Requests != 3 || asked.Load() != 3 || len(rec.got()) != 2 {
		t.Errorf("Requests=%d permit=%d waits=%v, want 3 attempts", res.Requests, asked.Load(), rec.got())
	}
}

func TestRetry_RecoversAfterADroppedConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close() // the provider hangs up without answering
		case 2:
			fmt.Fprint(w, pageBody(1))
		default:
			fmt.Fprint(w, `{"status":"OK","data":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, rec := newTestClient(t, srv.URL)

	res, err := c.SearchPages(context.Background(), criteria, 0, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if len(res.Properties) != 1 || res.Requests != 3 {
		t.Errorf("props=%d Requests=%d, want 1 and 3", len(res.Properties), res.Requests)
	}
	if got, want := rec.got(), []time.Duration{time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestRetry_TruncatedBodyIsTransient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		// Promise more bytes than are sent, then hang up: the read fails.
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, `{"status":"OK","da`)
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv.URL)

	res, err := c.SearchPages(context.Background(), criteria, 0, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !IsTransient(err) {
		t.Errorf("a response cut off mid-body is a network failure, not a decode error: %v", err)
	}
	if res.Requests != 3 || calls.Load() != 3 {
		t.Errorf("Requests=%d calls=%d, want 3 and 3", res.Requests, calls.Load())
	}
}

func TestRetry_ClientTimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers; returns once the client gives up
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", 30*time.Millisecond)
	rec := &waitRecorder{}
	c.wait = rec.wait

	res, err := c.SearchPages(context.Background(), criteria, 0, nil)
	if err == nil {
		t.Fatal("want a timeout")
	}
	if !IsTransient(err) {
		t.Errorf("a request timeout must be transient: %v", err)
	}
	if res.Requests != 3 {
		t.Errorf("Requests=%d, want 3 (a per-request timeout is retried)", res.Requests)
	}
}

func TestRetry_CancelDuringWaitReturnsPromptly(t *testing.T) {
	srv, calls := scripted(t,
		step{code: http.StatusServiceUnavailable},
		step{code: http.StatusOK, body: pageBody(1)}, // must never be reached
	)
	c := New(srv.URL, "k", 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	realWait := c.wait
	c.wait = func(ctx context.Context, d time.Duration) error {
		// The shutdown arrives while the client is sleeping off the 503.
		time.AfterFunc(20*time.Millisecond, cancel)
		return realWait(ctx, d)
	}

	start := time.Now()
	res, err := c.SearchPages(ctx, criteria, 0, nil)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("returned after %v; the 1s back-off must be abandoned on cancel", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Errorf("err = %v, should still carry the 503 that was being retried", err)
	}
	if IsTransient(err) {
		t.Error("a cancelled wait is a shutdown, not a transient provider failure")
	}
	if res.Requests != 1 || calls.Load() != 1 || res.NextPage != 1 {
		t.Errorf("Requests=%d calls=%d NextPage=%d, want 1, 1, 1", res.Requests, calls.Load(), res.NextPage)
	}
}

// A ledger fails closed, so under a dead context its denial says nothing
// about the budget. Reporting it as "budget exhausted" would make a shutdown
// look like a clean, resumable stop.
func TestRetry_DeadContextIsNotBudgetExhaustion(t *testing.T) {
	srv, calls := scripted(t)
	c, _ := newTestClient(t, srv.URL)

	t.Run("cancelled before the attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		permit, asked := budget(0)

		res, err := c.SearchPages(ctx, criteria, 0, permit)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if asked.Load() != 0 || res.Requests != 0 || res.NextPage != 1 {
			t.Errorf("permit=%d Requests=%d NextPage=%d, want 0, 0, 1", asked.Load(), res.Requests, res.NextPage)
		}
	})

	t.Run("cancelled inside the permit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		permit := func(context.Context) bool {
			cancel()
			return false
		}

		_, _, err := c.PropertyDetails(ctx, "1", permit)
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBudgetExhausted) {
			t.Errorf("err = %v, want context.Canceled and not ErrBudgetExhausted", err)
		}
	})

	if calls.Load() != 0 {
		t.Errorf("%d requests were sent under a dead context", calls.Load())
	}
}

// A request that cannot be sent is a configuration bug: it must not spend
// budget, must not be retried and must not read as a provider outage. Most of
// these base URLs PARSE, so http.NewRequest accepts them and only the
// transport refuses, with a *url.Error that looks like any network failure:
// against a ledger that was three units per call for nothing on the wire.
func TestRetry_UnsendableRequestSpendsNothing(t *testing.T) {
	bases := map[string]string{
		"does not parse":           "http://bad host",
		"scheme forgotten":         "api.openwebninja.com/realtime-zillow-data",
		"host:port read as scheme": "localhost:8080/realtime-zillow-data",
		"not http":                 "ftp://api.openwebninja.com/realtime-zillow-data",
		"no host":                  "https:///realtime-zillow-data",
		"empty":                    "",
	}
	for name, base := range bases {
		t.Run(name, func(t *testing.T) {
			calls := map[string]func(*Client, Permit) (int, error){
				"search": func(c *Client, permit Permit) (int, error) {
					res, err := c.SearchPages(context.Background(), criteria, 0, permit)
					if res.NextPage != 1 || len(res.Properties) != 0 {
						t.Errorf("result = %+v, want nothing fetched and NextPage 1", res)
					}
					return res.Requests, err
				},
				"details": func(c *Client, permit Permit) (int, error) {
					_, _, err := c.PropertyDetails(context.Background(), "1", permit)
					return 0, err
				},
			}
			for endpoint, call := range calls {
				c, rec := newTestClient(t, base)
				permit, asked := budget(100)

				requests, err := call(c, permit)
				if err == nil {
					t.Fatalf("%s: want an error for base URL %q", endpoint, base)
				}
				if IsTransient(err) {
					t.Errorf("%s: an unsendable URL is not transient: %v", endpoint, err)
				}
				if asked.Load() != 0 || requests != 0 || len(rec.got()) != 0 {
					t.Errorf("%s: permit=%d Requests=%d waits=%v, want nothing spent", endpoint, asked.Load(), requests, rec.got())
				}
			}
		})
	}
}

// StatusError.Body is documented as bounded: it ends up in every log line
// about the failure, and a gateway's HTML error page can be far larger.
func TestRetry_StatusErrorBodyIsBounded(t *testing.T) {
	srv, _ := scripted(t, step{code: http.StatusBadRequest, body: strings.Repeat("x", 4096)})
	c, _ := newTestClient(t, srv.URL)

	_, err := c.SearchPages(context.Background(), criteria, 0, nil)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if len(se.Body) != 512 {
		t.Errorf("len(Body) = %d, want the first 512 bytes of a 4096-byte answer", len(se.Body))
	}
}

func TestEmptyData(t *testing.T) {
	for raw, want := range map[string]bool{
		"":             true, // absent
		"null":         true,
		"[]":           true,
		"[ ]":          true,
		"{}":           true,
		"{\n}":         true,
		`""`:           true,
		"[{}]":         false,
		`{"zpid":"1"}`: false,
		"0":            false,
	} {
		if got := emptyData(json.RawMessage(raw)); got != want {
			t.Errorf("emptyData(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Errorf("an elapsed wait returned %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("a cancelled wait took %v", elapsed)
	}
}

func TestStatusError_Message(t *testing.T) {
	cases := map[string]*StatusError{
		"zillow API returned status 429: Too Many Requests": {Code: 429, Body: "Too Many Requests"},
		"zillow API returned status 502":                    {Code: 502},
	}
	for want, se := range cases {
		if got := se.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
}

func TestIsTransient(t *testing.T) {
	_, decodeErr := decodeEnvelope([]byte(`{"status":`))
	if decodeErr == nil {
		t.Fatal("test setup: want a decode error")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(decodeErr, &syntaxErr) {
		t.Fatalf("test setup: decode error is %T", decodeErr)
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"budget exhausted", ErrBudgetExhausted, false},
		{"wrapped budget exhausted", fmt.Errorf("details zpid=1: %w", ErrBudgetExhausted), false},
		{"details not found", fmt.Errorf("zpid=1: %w", ErrDetailsNotFound), false},
		{"context canceled", context.Canceled, false},
		{"request aborted by cancel", &url.Error{Op: "Get", URL: "http://x", Err: context.Canceled}, false},
		{"cancelled while retrying a 503", fmt.Errorf("%w (after: %w)", context.Canceled, &StatusError{Code: 503}), false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"429", &StatusError{Code: 429}, true},
		{"500", &StatusError{Code: 500}, true},
		{"wrapped 503", fmt.Errorf("search page 3: %w", &StatusError{Code: 503}), true},
		{"599", &StatusError{Code: 599}, true},
		{"400", &StatusError{Code: 400}, false},
		{"401", &StatusError{Code: 401}, false},
		{"403", &StatusError{Code: 403}, false},
		{"404", &StatusError{Code: 404}, false},
		{"422", &StatusError{Code: 422}, false},
		{"302 without a location", &StatusError{Code: 302}, false},
		{"soft envelope error", fmt.Errorf("search page 1: %w", errSoftEnvelope), true},
		{"connection refused", &url.Error{Op: "Get", URL: "http://x", Err: errors.New("connection refused")}, true},
		{"malformed URL", &url.Error{Op: "parse", URL: "http://bad host", Err: errors.New("invalid character")}, false},
		{"dial error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")}, true},
		{"dns error", &net.DNSError{Err: "no such host", Name: "api.example"}, true},
		{"body cut short", fmt.Errorf("%w: %w", errBodyRead, io.ErrUnexpectedEOF), true},
		{"decode error", decodeErr, false},
		{"anything else", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("%s: IsTransient(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestUsage_IsUnmeteredAndSingleAttempt(t *testing.T) {
	var calls atomic.Int32
	var gotPath, gotAPIID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAPIID = r.URL.Path, r.URL.Query().Get("api_id")
		if calls.Add(1) == 1 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"status":"OK","data":{"status":"ok","quotas":[{"name":"Requests","limit":100,"used":40,"remaining":60}]}}`)
	}))
	t.Cleanup(srv.Close)
	c, rec := newTestClient(t, srv.URL+"/realtime-zillow-data")

	_, err := c.Usage(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a *StatusError with Code 503", err)
	}
	if calls.Load() != 1 || len(rec.got()) != 0 {
		t.Errorf("calls=%d waits=%v: the quota probe fails open, so it is never retried", calls.Load(), rec.got())
	}

	u, err := c.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if gotPath != "/usage" || gotAPIID != "realtime_zillow_data" {
		t.Errorf("request wrong: path=%q api_id=%q", gotPath, gotAPIID)
	}
	if u.Status != "ok" || len(u.Quotas) != 1 || u.Quotas[0].Remaining != 60 {
		t.Errorf("usage decoded wrong: %+v", u)
	}
}
