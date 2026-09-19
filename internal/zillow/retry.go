package zillow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Permit is asked for one request immediately before every paid HTTP attempt,
// retries included, and reports whether the attempt may be sent. In production
// it reserves from the fleet-wide budget ledger, which has no refunds: asking
// per attempt (rather than per page or per call) is what keeps the ledger
// equal to what the provider actually billed. A nil Permit always allows.
type Permit func(ctx context.Context) bool

// ErrBudgetExhausted means the Permit denied an attempt; nothing was sent for
// it. It is a scheduling signal, not a provider failure, so IsTransient is
// false for it.
var ErrBudgetExhausted = errors.New("zillow API budget exhausted")

// StatusError is any non-200 answer from the provider. It is typed so callers
// can tell a 429 (close the quota gate) and a 5xx (back off, do not blame the
// row) from a 4xx that will never succeed.
type StatusError struct {
	Code int
	Body string // at most 512 bytes, trimmed; may be empty
}

// Error includes the API's error body (e.g. "Too Many Requests") so quota vs.
// rate-limit vs. auth failures are distinguishable from the logs alone.
func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("zillow API returned status %d", e.Code)
	}
	return fmt.Sprintf("zillow API returned status %d: %s", e.Code, e.Body)
}

var (
	// errSoftEnvelope is a 200 whose envelope reports a failure and carries
	// no usable data. Without it a provider hiccup would read as "no
	// listings" (the ZIP gets marked searched), "not found" or an all-empty
	// details record (the row gets stamped fetched), and all of those are
	// permanent.
	errSoftEnvelope = errors.New("zillow API reported an error instead of data")

	// errBodyRead marks a 200 whose body could not be read to the end. It
	// keeps a connection dropped mid-response on the network side of
	// IsTransient instead of surfacing later as a JSON decode error.
	errBodyRead = errors.New("read zillow response")
)

// IsTransient reports whether err is the provider or the network misbehaving
// (transport errors, timeouts, 429, 5xx, a soft envelope error) rather than
// something wrong with this particular request. Callers use it to decide
// whether a failure counts against the row: a transient one must not, or an
// outage would abandon healthy rows.
//
// A cancelled context is a shutdown, not a provider failure, and an exhausted
// budget or a not-found are answers, not failures: all three are false.
func IsTransient(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, context.Canceled),
		errors.Is(err, ErrBudgetExhausted),
		errors.Is(err, ErrDetailsNotFound):
		return false
	case errors.Is(err, errSoftEnvelope),
		errors.Is(err, errBodyRead),
		errors.Is(err, context.DeadlineExceeded):
		return true
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	// Everything http.Client.Do returns is a *url.Error: DNS, refused and
	// reset connections, TLS failures, the per-request timeout. So is a URL
	// that does not parse, and no amount of retrying fixes a bad base URL.
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op != "parse"
	}
	var ne net.Error
	return errors.As(err, &ne)
}

const (
	// maxRetryAfter bounds how long the provider can park a worker: the wait
	// happens under a ZIP or details claim whose work deadline keeps running.
	maxRetryAfter = 60 * time.Second

	// maxBodyBytes caps a response held in memory. A search page with full
	// photo carousels is ~200 KiB and a details record less; 16 MiB is ample.
	maxBodyBytes = 16 << 20
)

// retryBackoff is the wait before the 2nd and the 3rd attempt when the
// provider sent no usable Retry-After. Its length fixes the attempt count.
var retryBackoff = [...]time.Duration{1 * time.Second, 3 * time.Second}

// fetch is the metered GET shared by search and details: up to three attempts
// on transport errors, 429 and 5xx, each one preceded by the permit. The int
// is the number of requests that actually went out, which is also the number
// of permits granted; it is reported on every path because a failed request
// still counted against the provider's quota.
func (c *Client) fetch(ctx context.Context, endpoint string, permit Permit) ([]byte, int, error) {
	// Built, and checked for sendability, before the first permit so a
	// misconfigured base URL never spends budget.
	req, err := c.newRequest(ctx, endpoint)
	if err != nil {
		return nil, 0, err
	}

	var (
		sent    int
		lastErr error
	)
	for attempt := 0; ; attempt++ {
		// A ledger-backed permit fails closed, so under a dead context its
		// denial says nothing about the budget. Checking the context on both
		// sides of it keeps a shutdown from looking like a clean, resumable
		// "budget ran out".
		if err := ctx.Err(); err != nil {
			return nil, sent, abandoned(err, lastErr)
		}
		if permit != nil && !permit(ctx) {
			if err := ctx.Err(); err != nil {
				return nil, sent, abandoned(err, lastErr)
			}
			if lastErr != nil {
				return nil, sent, fmt.Errorf("%w (giving up on: %v)", ErrBudgetExhausted, lastErr)
			}
			return nil, sent, ErrBudgetExhausted
		}

		sent++
		body, retryAfter, err := c.do(req)
		if err == nil {
			return body, sent, nil
		}
		if !IsTransient(err) || attempt >= len(retryBackoff) {
			return nil, sent, err
		}
		lastErr = err

		delay := retryBackoff[attempt]
		if retryAfter > 0 {
			delay = min(retryAfter, maxRetryAfter)
		}
		if err := c.wait(ctx, delay); err != nil {
			return nil, sent, abandoned(err, lastErr)
		}
	}
}

// abandoned reports a context that ended between attempts. The failure that
// was being retried stays in the chain so a caller can still see, say, the
// 429 that should close the quota gate.
func abandoned(ctxErr, lastErr error) error {
	switch {
	case lastErr == nil:
		return ctxErr
	case errors.Is(lastErr, ctxErr):
		return lastErr // the attempt itself died of the context: say it once
	}
	return fmt.Errorf("%w (while retrying: %w)", ctxErr, lastErr)
}

// newRequest builds the GET for endpoint, or fails without anything having
// been spent when it could never be sent.
func (c *Client) newRequest(ctx context.Context, endpoint string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// http.NewRequest only parses. A base URL with the scheme forgotten
	// ("api.example.com/x"), a "host:port/x" (which parses as scheme "host")
	// or one without a host is refused later, by the transport, and that
	// refusal is a *url.Error like any network failure: it would be retried
	// at one permit per attempt and reported as a provider outage with
	// nothing on the wire. Nothing validates ZILLOW_BASE_URL upstream, so it
	// is refused here with an error IsTransient does not recognise.
	if u := req.URL; (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("build request: base URL %q is not an absolute http(s) URL", c.baseURL)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// do sends req once and returns the body of a 200. retryAfter is the
// provider's Retry-After on a non-200, zero when absent or unusable. req has
// no body, so fetch may send the same one again once this call has returned.
func (c *Client) do(req *http.Request) (body []byte, retryAfter time.Duration, err error) {
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("zillow request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, parseRetryAfter(res.Header.Get("Retry-After")),
			&StatusError{Code: res.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	body, err = io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errBodyRead, err)
	}
	return body, 0, nil
}

// parseRetryAfter reads the integer-seconds form of Retry-After. The HTTP-date
// form, garbage, and values <= 0 all mean "no guidance" and return 0, which
// leaves the default back-off in force: a retry costs a budgeted request, so
// it is never fired without a pause.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	if secs > int(maxRetryAfter/time.Second) {
		return maxRetryAfter // also keeps a huge value from overflowing Duration
	}
	return time.Duration(secs) * time.Second
}

// sleepCtx waits for d or until ctx is done, whichever comes first. It is the
// production value of Client.wait; tests swap in a recorder.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// envelope is the wrapper every OpenWebNinja endpoint answers with:
// {status, request_id, parameters, data}. Data stays raw so the status can be
// judged before the payload's shape is trusted.
type envelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

// failed reports whether the provider flagged the response: a status that is
// present and is not "OK". An absent status is not a failure, because the
// envelope has not always been relied on to carry one.
func (e envelope) failed() bool {
	status := strings.TrimSpace(e.Status)
	return status != "" && !strings.EqualFold(status, "OK")
}

// softError is errSoftEnvelope for a failed envelope, with enough of the body
// to see in the logs what the provider complained about.
func (e envelope) softError(body []byte) error {
	return fmt.Errorf("%w: status %q: %s", errSoftEnvelope, strings.TrimSpace(e.Status), excerpt(body))
}

// decodeEnvelope unwraps a 200 body. A failed envelope with empty data is
// errSoftEnvelope; an "OK" (or absent) status with empty data is returned as
// is, because there it keeps its endpoint-specific meaning (end of results,
// details not found).
//
// The envelope is returned rather than just its data because "empty" is not
// the only way a failed response carries nothing: the failure report may sit
// in data itself ({"message": ...}). Whether data holds a usable payload is
// the endpoint's call, so search and details check failed() once they have
// tried to decode it. A payload that did arrive is never discarded over a
// status string.
func decodeEnvelope(body []byte) (envelope, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return envelope{}, fmt.Errorf("decode zillow response: %w", err)
	}
	if env.failed() && emptyData(env.Data) {
		return envelope{}, env.softError(body)
	}
	return env, nil
}

// emptyData reports whether an envelope's data carries nothing: absent, null,
// or an empty array, object or string.
func emptyData(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true // the envelope had no "data" key at all
	}
	if len(raw) > 64 {
		return false // anything this long holds data; skip the compaction
	}
	// Compacting makes "[ ]" and "[]" the same thing.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return false
	}
	switch compact.String() {
	case "null", "[]", "{}", `""`:
		return true
	}
	return false
}

// excerpt bounds a response body for an error message, so the log line says
// what the provider complained about without carrying a whole payload.
func excerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}
