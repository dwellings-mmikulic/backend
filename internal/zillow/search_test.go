package zillow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/config"
)

// searchServer serves n pages of one listing each, then an empty page.
func searchServer(t *testing.T, pagesWithData int, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		page := r.URL.Query().Get("page")
		var p int
		fmt.Sscanf(page, "%d", &p)
		if p > pagesWithData {
			fmt.Fprint(w, `{"status":"OK","data":[]}`)
			return
		}
		fmt.Fprint(w, pageBody(p))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// zpids lists the result's zpids in order, to assert which pages it holds.
func zpids(res SearchResult) []string {
	out := make([]string, 0, len(res.Properties))
	for _, p := range res.Properties {
		out = append(out, p.ZPID)
	}
	return out
}

func TestSearchPages_MaxPagesLimitsRequests(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls) // always has data
	c := New(srv.URL, "k", time.Second)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxPages: 2}, 0, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if res.Requests != 2 || calls.Load() != 2 {
		t.Errorf("Requests=%d calls=%d, want 2 and 2", res.Requests, calls.Load())
	}
	if len(res.Properties) != 2 {
		t.Errorf("got %d props, want 2", len(res.Properties))
	}
	if res.NextPage != 0 {
		t.Errorf("NextPage=%d, want 0 (reaching the page cap completes the search)", res.NextPage)
	}
}

func TestSearchPages_StopsOnEmptyPageAndCountsIt(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 1, &calls) // page 1 has data, page 2 empty
	c := New(srv.URL, "k", time.Second)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if res.Requests != 2 {
		t.Errorf("Requests=%d, want 2 (data page + empty page both cost a request)", res.Requests)
	}
	if len(res.Properties) != 1 {
		t.Errorf("got %d props, want 1", len(res.Properties))
	}
	if res.NextPage != 0 {
		t.Errorf("NextPage=%d, want 0 (an empty page completes the search)", res.NextPage)
	}
}

func TestSearchPages_ErrorStillReportsRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv.URL)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
	if err == nil {
		t.Fatal("want error")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTooManyRequests || se.Body != "Too Many Requests" {
		t.Errorf("err = %v, want a *StatusError carrying the 429 and its body", err)
	}
	if res.Requests != 3 {
		t.Errorf("Requests=%d, want 3 (every failed attempt was still made)", res.Requests)
	}
	if res.NextPage != 1 {
		t.Errorf("NextPage=%d, want 1", res.NextPage)
	}
}

func TestSearchPages_MaxResultsStillHonored(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls)
	c := New(srv.URL, "k", time.Second)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxResults: 1}, 0, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if len(res.Properties) != 1 {
		t.Errorf("got %d props, want 1", len(res.Properties))
	}
	if res.Requests != 1 || res.NextPage != 0 {
		t.Errorf("Requests=%d NextPage=%d, want 1 and 0 (reaching MaxResults completes the search)", res.Requests, res.NextPage)
	}
}

func TestSearchPages_HardCapBindsAt20(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls) // always has data
	c := New(srv.URL, "k", time.Second)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxPages: 25}, 0, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if res.Requests != 20 || calls.Load() != 20 {
		t.Errorf("Requests=%d calls=%d, want 20 and 20 (MaxPages: 25 capped by hard limit)", res.Requests, calls.Load())
	}
	if len(res.Properties) != 20 {
		t.Errorf("got %d props, want 20", len(res.Properties))
	}
}

func TestSearchPages_UnsetMaxPagesStopsAtHardCap(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls) // always has data
	c := New(srv.URL, "k", time.Second)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if res.Requests != 20 || calls.Load() != 20 {
		t.Errorf("Requests=%d calls=%d, want 20 and 20 (unset MaxPages defaults to hard cap)", res.Requests, calls.Load())
	}
	if len(res.Properties) != 20 {
		t.Errorf("got %d props, want 20", len(res.Properties))
	}
	if res.NextPage != 0 {
		t.Errorf("NextPage=%d, want 0", res.NextPage)
	}
}

// The pages already paid for must survive a later failure: the scheduler
// enqueues them and resumes the ZIP at NextPage instead of buying them again.
func TestSearchPages_ErrorKeepsThePagesAlreadyFetched(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var p int
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &p)
		if p == 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if p > 5 {
			fmt.Fprint(w, `{"status":"OK","data":[]}`)
			return
		}
		fmt.Fprint(w, pageBody(p))
	}))
	t.Cleanup(srv.Close)
	c, rec := newTestClient(t, srv.URL)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusInternalServerError {
		t.Fatalf("err = %v, want a *StatusError with Code 500", err)
	}
	if got, want := zpids(res), []string{"z1", "z2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("properties = %v, want %v (pages 1-2 are returned with the error)", got, want)
	}
	if res.NextPage != 3 {
		t.Errorf("NextPage=%d, want 3 (the failing page)", res.NextPage)
	}
	if res.Requests != 5 || calls.Load() != 5 {
		t.Errorf("Requests=%d calls=%d, want 5 and 5 (pages 1, 2 and three attempts at page 3)", res.Requests, calls.Load())
	}
	if got, want := rec.got(), []time.Duration{time.Second, 3 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

// A denied permit is the budget running out mid-ZIP: not a failure, nothing
// sent for that page, and the caller is told where to resume.
func TestSearchPages_PermitDeniedMidPagination(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls)
	c := New(srv.URL, "k", time.Second)
	permit, asked := budget(2)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, permit)
	if err != nil {
		t.Fatalf("a denied permit must not be an error, got %v", err)
	}
	if got, want := zpids(res), []string{"z1", "z2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("properties = %v, want %v", got, want)
	}
	if res.NextPage != 3 {
		t.Errorf("NextPage=%d, want 3 (the first unfetched page)", res.NextPage)
	}
	if res.Requests != 2 || calls.Load() != 2 {
		t.Errorf("Requests=%d calls=%d, want 2 and 2 (nothing is sent for the denied page)", res.Requests, calls.Load())
	}
	if asked.Load() != 3 {
		t.Errorf("permit consulted %d times, want 3 (once per attempt, the denial included)", asked.Load())
	}
}

func TestSearchPages_PermitDeniedBeforeTheFirstPage(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls)
	c := New(srv.URL, "k", time.Second)
	permit, _ := budget(0)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 4, permit)
	if err != nil {
		t.Fatalf("a denied permit must not be an error, got %v", err)
	}
	if len(res.Properties) != 0 || res.NextPage != 4 || res.Requests != 0 || calls.Load() != 0 {
		t.Errorf("result = %+v calls=%d, want nothing fetched and NextPage 4", res, calls.Load())
	}
}

func TestSearchPages_ResumesFromStartPage(t *testing.T) {
	var mu sync.Mutex
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		mu.Lock()
		pages = append(pages, page)
		mu.Unlock()
		var p int
		fmt.Sscanf(page, "%d", &p)
		if p > 4 {
			fmt.Fprint(w, `{"status":"OK","data":[]}`)
			return
		}
		fmt.Fprint(w, pageBody(p))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", time.Second)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 3, nil)
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"3", "4", "5"}; !reflect.DeepEqual(pages, want) {
		t.Errorf("requested pages %v, want %v (page 3 first: 1-2 were paid for in an earlier window)", pages, want)
	}
	if got, want := zpids(res), []string{"z3", "z4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("properties = %v, want %v", got, want)
	}
	if res.Requests != 3 || res.NextPage != 0 {
		t.Errorf("Requests=%d NextPage=%d, want 3 and 0", res.Requests, res.NextPage)
	}
}

// MaxPages is an absolute page number, not a per-call allowance: a resumed
// search must stop at the same page an uninterrupted one would have.
func TestSearchPages_PageCapIsAbsoluteWhenResuming(t *testing.T) {
	cases := map[string]struct {
		startPage    int
		wantRequests int32
	}{
		"resume inside the cap":  {startPage: 2, wantRequests: 2}, // pages 2 and 3
		"resume at the cap":      {startPage: 3, wantRequests: 1},
		"resume past the cap":    {startPage: 4, wantRequests: 0},
		"negative means page 1":  {startPage: -1, wantRequests: 3},
		"zero also means page 1": {startPage: 0, wantRequests: 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := searchServer(t, 100, &calls)
			c := New(srv.URL, "k", time.Second)

			res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxPages: 3}, tc.startPage, nil)
			if err != nil {
				t.Fatalf("SearchPages: %v", err)
			}
			if calls.Load() != tc.wantRequests || res.Requests != int(tc.wantRequests) || res.NextPage != 0 {
				t.Errorf("calls=%d Requests=%d NextPage=%d, want %d requests and a complete search", calls.Load(), res.Requests, res.NextPage, tc.wantRequests)
			}
		})
	}
}

// A 200 that says "not OK" and carries no data is the provider failing, not
// the end of the results: treating it as an empty page would mark a ZIP
// searched with listings missing.
func TestSearchPages_SoftEnvelopeError(t *testing.T) {
	bodies := map[string]string{
		"empty array":  `{"status":"ERROR","request_id":"r1","data":[]}`,
		"null data":    `{"status":"ERROR","data":null}`,
		"absent data":  `{"status":"error","message":"upstream timeout"}`,
		"empty object": `{"status":"FAILED","data":{}}`,
		"empty string": `{"status":"FAILED","data":""}`,
		// The failure report may itself sit in data: still no listings, and
		// still the provider's failure rather than a malformed page.
		"error object": `{"status":"ERROR","request_id":"r1","data":{"message":"upstream timeout"}}`,
		"error string": `{"status":"ERROR","data":"upstream timeout"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, calls := scripted(t,
				step{code: http.StatusOK, body: pageBody(1)},
				step{code: http.StatusOK, body: body},
			)
			c, rec := newTestClient(t, srv.URL)

			res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
			if err == nil {
				t.Fatal("want an error, got a clean end of results")
			}
			if !IsTransient(err) {
				t.Errorf("soft envelope error must be transient: %v", err)
			}
			if len(res.Properties) != 1 || res.NextPage != 2 {
				t.Errorf("props=%d NextPage=%d, want page 1 kept and NextPage 2", len(res.Properties), res.NextPage)
			}
			if res.Requests != 2 || calls.Load() != 2 || len(rec.got()) != 0 {
				t.Errorf("Requests=%d calls=%d waits=%v, want 2 requests and no in-client retry", res.Requests, calls.Load(), rec.got())
			}
		})
	}
}

func TestSearchPages_EnvelopeStatusThatIsNotAnError(t *testing.T) {
	cases := map[string]struct {
		body      string
		wantProps int
	}{
		"lower-case ok, empty page": {`{"status":"ok","data":[]}`, 0},
		"absent status, empty page": {`{"data":[]}`, 0},
		"OK with null data":         {`{"status":"OK","data":null}`, 0},
		"OK with absent data":       {`{"status":"OK","request_id":"r1"}`, 0},
		"padded ok, empty page":     {`{"status":" OK ","data":[]}`, 0},
		"blank status, empty page":  {`{"status":"  ","data":[]}`, 0},
		// The rule needs BOTH a bad status and no data: listings that did
		// arrive are never thrown away over a status string.
		"odd status with data": {`{"status":"PARTIAL","data":[{"zpid":"z9","price":1}]}`, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := scripted(t, step{code: http.StatusOK, body: tc.body})
			c, _ := newTestClient(t, srv.URL)

			res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
			if err != nil {
				t.Fatalf("SearchPages: %v", err)
			}
			if len(res.Properties) != tc.wantProps || res.NextPage != 0 {
				t.Errorf("props=%d NextPage=%d, want %d and 0", len(res.Properties), res.NextPage, tc.wantProps)
			}
		})
	}
}

// Only an array, null or no data at all ends a search. Anything else under an
// OK status is a response shape we do not know, and it has to fail loudly: a
// clean end of results marks the ZIP searched, so a provider that one day
// moves its listings under data.results would silently empty every ZIP.
func TestSearchPages_UnknownDataShapeIsNotTheEndOfResults(t *testing.T) {
	bodies := map[string]string{
		"empty object":   `{"status":"OK","data":{}}`,
		"empty string":   `{"status":"OK","data":""}`,
		"nested results": `{"status":"OK","data":{"results":[{"zpid":"z9"}]}}`,
		"absent status":  `{"data":{}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, calls := scripted(t,
				step{code: http.StatusOK, body: pageBody(1)},
				step{code: http.StatusOK, body: body},
			)
			c, rec := newTestClient(t, srv.URL)

			res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
			if err == nil {
				t.Fatal("want a decode error, got a clean end of results")
			}
			if IsTransient(err) {
				t.Errorf("an unknown response shape is not retryable: %v", err)
			}
			if got, want := zpids(res), []string{"z1"}; !reflect.DeepEqual(got, want) || res.NextPage != 2 {
				t.Errorf("properties=%v NextPage=%d, want %v kept and NextPage 2", got, res.NextPage, want)
			}
			if res.Requests != 2 || calls.Load() != 2 || len(rec.got()) != 0 {
				t.Errorf("Requests=%d calls=%d waits=%v, want 2 requests and no retry", res.Requests, calls.Load(), rec.got())
			}
		})
	}
}

func TestSearchPages_DecodeErrorIsNotTransient(t *testing.T) {
	srv, calls := scripted(t, step{code: http.StatusOK, body: `<html>gateway</html>`})
	c, _ := newTestClient(t, srv.URL)

	res, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"}, 0, nil)
	if err == nil {
		t.Fatal("want a decode error")
	}
	if IsTransient(err) {
		t.Errorf("a body that is not JSON is not retryable: %v", err)
	}
	if res.Requests != 1 || calls.Load() != 1 || res.NextPage != 1 {
		t.Errorf("Requests=%d calls=%d NextPage=%d, want 1, 1, 1", res.Requests, calls.Load(), res.NextPage)
	}
}
