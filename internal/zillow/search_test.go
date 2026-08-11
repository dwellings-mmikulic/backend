package zillow

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		fmt.Fprintf(w, `{"status":"OK","data":[{"zpid":"z%d","price":100000,"streetAddress":"s","city":"c","state":"FL","zipcode":"33950"}]}`, p)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSearchPages_MaxPagesLimitsRequests(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls) // always has data
	c := New(srv.URL, "k", time.Second)

	props, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxPages: 2})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if pages != 2 || calls.Load() != 2 {
		t.Errorf("pages=%d calls=%d, want 2 and 2", pages, calls.Load())
	}
	if len(props) != 2 {
		t.Errorf("got %d props, want 2", len(props))
	}
}

func TestSearchPages_StopsOnEmptyPageAndCountsIt(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 1, &calls) // page 1 has data, page 2 empty
	c := New(srv.URL, "k", time.Second)

	props, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if pages != 2 {
		t.Errorf("pages=%d, want 2 (data page + empty page both cost a request)", pages)
	}
	if len(props) != 1 {
		t.Errorf("got %d props, want 1", len(props))
	}
}

func TestSearchPages_ErrorStillReportsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", time.Second)

	_, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"})
	if err == nil {
		t.Fatal("want error")
	}
	if pages != 1 {
		t.Errorf("pages=%d, want 1 (the failed request was still made)", pages)
	}
}

func TestSearchPages_MaxResultsStillHonored(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls)
	c := New(srv.URL, "k", time.Second)

	props, _, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxResults: 1})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if len(props) != 1 {
		t.Errorf("got %d props, want 1", len(props))
	}
}

func TestSearchPages_HardCapBindsAt20(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls) // always has data
	c := New(srv.URL, "k", time.Second)

	props, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxPages: 25})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if pages != 20 || calls.Load() != 20 {
		t.Errorf("pages=%d calls=%d, want 20 and 20 (MaxPages: 25 capped by hard limit)", pages, calls.Load())
	}
	if len(props) != 20 {
		t.Errorf("got %d props, want 20", len(props))
	}
}

func TestSearchPages_UnsetMaxPagesStopsAtHardCap(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls) // always has data
	c := New(srv.URL, "k", time.Second)

	props, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if pages != 20 || calls.Load() != 20 {
		t.Errorf("pages=%d calls=%d, want 20 and 20 (unset MaxPages defaults to hard cap)", pages, calls.Load())
	}
	if len(props) != 20 {
		t.Errorf("got %d props, want 20", len(props))
	}
}

func TestSearchPages_ErrorAfterPartialSuccessReportsAllPages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		page := r.URL.Query().Get("page")
		var p int
		fmt.Sscanf(page, "%d", &p)
		if p <= 2 {
			fmt.Fprintf(w, `{"status":"OK","data":[{"zpid":"z%d","price":100000,"streetAddress":"s","city":"c","state":"FL","zipcode":"33950"}]}`, p)
			return
		}
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", time.Second)

	_, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"})
	if err == nil {
		t.Fatal("want error after page 2 fails")
	}
	if pages != 3 {
		t.Errorf("pages=%d, want 3 (pages 1, 2 succeeded, page 3 failed; distinguishes from off-by-one reporting 1)", pages)
	}
}
