package zillow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// TestPropertyDetails_MapsFixture drives the client against a fake server
// returning the pinned fixture and checks the full Details mapping.
func TestPropertyDetails_MapsFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/property_details.json")
	if err != nil {
		t.Fatal(err)
	}

	var gotPath, gotZPID, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotZPID = r.URL.Query().Get("zpid")
		gotKey = r.Header.Get("X-API-Key")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key", 5*time.Second)
	d, raw, err := c.PropertyDetails(context.Background(), "43590635", nil)
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/property-details" || gotZPID != "43590635" || gotKey != "test-key" {
		t.Errorf("request wrong: path=%q zpid=%q key=%q", gotPath, gotZPID, gotKey)
	}
	if string(raw) != string(fixture) {
		t.Error("raw body must be returned verbatim for details_raw storage")
	}

	check := func(name string, got *string, want string) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %q", name, got, want)
		}
	}
	check("PropertyType", d.PropertyType, "SINGLE_FAMILY")
	check("ListingStatus", d.ListingStatus, "FOR_SALE")
	check("Description", d.Description, "Stunning modern home with breathtaking Hill Country views.")
	check("Heating", d.Heating, "Central")
	check("Cooling", d.Cooling, "Central Air, Ceiling Fan(s)")
	check("Garage", d.Garage, "2 Car Garage")
	check("MLSNumber", d.MLSNumber, "1234567")
	check("AgentName", d.AgentName, "Hill Country Dream Realty")
	check("AgentPhone", d.AgentPhone, "512-555-0123")
	check("AgentBrokerage", d.AgentBrokerage, "Dream Brokerage LLC")

	if d.YearBuilt == nil || *d.YearBuilt != 2021 {
		t.Errorf("YearBuilt = %v, want 2021", d.YearBuilt)
	}
	if d.HOAFeeMonthly == nil || *d.HOAFeeMonthly != 125 {
		t.Errorf("HOAFeeMonthly = %v, want 125", d.HOAFeeMonthly)
	}
	if d.Latitude == nil || *d.Latitude != 30.2672 || d.Longitude == nil || *d.Longitude != -97.7431 {
		t.Errorf("lat/long = %v/%v", d.Latitude, d.Longitude)
	}
}

// TestPropertyDetails_EmptyFieldsAreNil verifies absent fields map to nil,
// not pointers to zero values.
func TestPropertyDetails_EmptyFieldsAreNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"OK","data":{"zpid":"1"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", 5*time.Second)
	d, _, err := c.PropertyDetails(context.Background(), "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.PropertyType != nil || d.Description != nil || d.YearBuilt != nil ||
		d.Heating != nil || d.Garage != nil || d.HOAFeeMonthly != nil ||
		d.AgentName != nil || d.Latitude != nil {
		t.Errorf("absent fields must be nil: %+v", d)
	}
}

func TestPropertyDetails_NotFound(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"http 404": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"null data": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"OK","data":null}`))
		},
		"lower-case ok, null data": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok","data":null}`))
		},
		"absent status and data": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"request_id":"r1"}`))
		},
		"padded ok, null data": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":" OK ","data":null}`))
		},
		"blank status, null data": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"  ","data":null}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				h(w, r)
			}))
			defer srv.Close()
			c, rec := newTestClient(t, srv.URL)
			permit, asked := budget(100)

			_, _, err := c.PropertyDetails(context.Background(), "gone", permit)
			if !errors.Is(err, ErrDetailsNotFound) {
				t.Errorf("err = %v, want ErrDetailsNotFound", err)
			}
			if IsTransient(err) {
				t.Error("not-found is final, never transient")
			}
			// A zpid the provider does not know costs exactly one request.
			if calls.Load() != 1 || asked.Load() != 1 || len(rec.got()) != 0 {
				t.Errorf("calls=%d permit=%d waits=%v, want one request and no retry", calls.Load(), asked.Load(), rec.got())
			}
		})
	}
}

func TestPropertyDetails_ServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv.URL)
	permit, asked := budget(100)

	_, _, err := c.PropertyDetails(context.Background(), "1", permit)
	if err == nil || errors.Is(err, ErrDetailsNotFound) {
		t.Fatalf("want a non-not-found error, got %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTooManyRequests || se.Body != "Too Many Requests" {
		t.Errorf("err = %v, want a *StatusError carrying the 429 and its body", err)
	}
	if !IsTransient(err) {
		t.Error("a 429 must be transient so the row is released, not counted as an attempt")
	}
	if calls.Load() != 3 || asked.Load() != 3 {
		t.Errorf("calls=%d permit=%d, want 3 and 3 (every retry reserves its own request)", calls.Load(), asked.Load())
	}
	if got, want := rec.got(), []time.Duration{time.Second, 3 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestPropertyDetails_ClientErrorFailsAtOnce(t *testing.T) {
	srv, calls := scripted(t, step{code: http.StatusForbidden, body: "bad key"})
	c, rec := newTestClient(t, srv.URL)

	_, _, err := c.PropertyDetails(context.Background(), "1", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("err = %v, want a *StatusError with Code 403", err)
	}
	if IsTransient(err) || errors.Is(err, ErrDetailsNotFound) {
		t.Errorf("a 403 is neither transient nor not-found: %v", err)
	}
	if calls.Load() != 1 || len(rec.got()) != 0 {
		t.Errorf("calls=%d waits=%v, want one request and no retry", calls.Load(), rec.got())
	}
}

func TestPropertyDetails_RetriesThenSucceeds(t *testing.T) {
	srv, calls := scripted(t,
		step{code: http.StatusBadGateway},
		step{code: http.StatusOK, body: `{"status":"OK","data":{"zpid":"1","homeType":"CONDO"}}`},
	)
	c, rec := newTestClient(t, srv.URL)

	d, raw, err := c.PropertyDetails(context.Background(), "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.PropertyType == nil || *d.PropertyType != "CONDO" || len(raw) == 0 {
		t.Errorf("details = %+v raw=%q", d, raw)
	}
	if calls.Load() != 2 || len(rec.got()) != 1 {
		t.Errorf("calls=%d waits=%v, want 2 requests and one wait", calls.Load(), rec.got())
	}
}

func TestPropertyDetails_PermitDenied(t *testing.T) {
	srv, calls := scripted(t)
	c, _ := newTestClient(t, srv.URL)
	permit, asked := budget(0)

	d, raw, err := c.PropertyDetails(context.Background(), "1", permit)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if d != nil || raw != nil {
		t.Errorf("a denied fetch returned data: %+v %q", d, raw)
	}
	if IsTransient(err) {
		t.Error("an exhausted budget is not a provider failure")
	}
	if calls.Load() != 0 || asked.Load() != 1 {
		t.Errorf("calls=%d permit=%d, want nothing sent and the permit asked once", calls.Load(), asked.Load())
	}
}

// A 200 that says "not OK" with no data is the provider failing. Reading it
// as not-found would stamp the row fetched and lose its details for good.
func TestPropertyDetails_SoftEnvelopeError(t *testing.T) {
	bodies := map[string]string{
		"null data":    `{"status":"ERROR","request_id":"r1","data":null}`,
		"absent data":  `{"status":"error","message":"upstream timeout"}`,
		"empty object": `{"status":"FAILED","data":{}}`,
		// The failure report may itself sit in data. It decodes into a record
		// with every field empty, which would be stored as the listing's
		// details: "no data" has to mean "no details record", not "no bytes".
		"error object":  `{"status":"ERROR","request_id":"r1","data":{"message":"upstream timeout"}}`,
		"error string":  `{"status":"ERROR","data":"upstream timeout"}`,
		"error list":    `{"status":"ERROR","data":["upstream timeout"]}`,
		"null zpid":     `{"status":"ERROR","data":{"zpid":null,"error":"upstream timeout"}}`,
		"padded status": `{"status":" Error ","data":{"code":504}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, calls := scripted(t, step{code: http.StatusOK, body: body})
			c, rec := newTestClient(t, srv.URL)

			d, raw, err := c.PropertyDetails(context.Background(), "1", nil)
			if err == nil || errors.Is(err, ErrDetailsNotFound) {
				t.Fatalf("err = %v, want an error that is not ErrDetailsNotFound", err)
			}
			if d != nil || raw != nil {
				t.Errorf("a failed fetch returned data: %+v %q", d, raw)
			}
			if !IsTransient(err) {
				t.Errorf("soft envelope error must be transient: %v", err)
			}
			if calls.Load() != 1 || len(rec.got()) != 0 {
				t.Errorf("calls=%d waits=%v, want one request and no in-client retry", calls.Load(), rec.got())
			}
		})
	}
}

// The rule needs BOTH a bad status and no record: a record that did arrive
// (it names its zpid) is stored whatever the status string says.
func TestPropertyDetails_OddStatusWithDataIsStored(t *testing.T) {
	for name, body := range map[string]string{
		"string zpid":  `{"status":"PARTIAL","data":{"zpid":"1","homeType":"CONDO"}}`,
		"numeric zpid": `{"status":"PARTIAL","data":{"zpid":1,"homeType":"CONDO"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := scripted(t, step{code: http.StatusOK, body: body})
			c, _ := newTestClient(t, srv.URL)

			d, raw, err := c.PropertyDetails(context.Background(), "1", nil)
			if err != nil {
				t.Fatal(err)
			}
			if d.PropertyType == nil || *d.PropertyType != "CONDO" || string(raw) != body {
				t.Errorf("details = %+v raw=%q", d, raw)
			}
		})
	}
}

// Under an OK (or absent) status nothing about the payload is second-guessed,
// as it never was: only absent/null data is not-found, and an object without
// the fields we map is a fetched record whose fields are all nil. Both stamp
// the row fetched, so neither may turn into a retry.
func TestPropertyDetails_OKStatusTrustsTheRecord(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":             `{"status":"OK","data":{}}`,
		"record without a zpid":    `{"status":"OK","data":{"homeType":""}}`,
		"absent status, no zpid":   `{"data":{"description":" "}}`,
		"lower-case ok, no fields": `{"status":"ok","data":{"resoFacts":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, calls := scripted(t, step{code: http.StatusOK, body: body})
			c, _ := newTestClient(t, srv.URL)

			d, raw, err := c.PropertyDetails(context.Background(), "1", nil)
			if err != nil {
				t.Fatalf("err = %v, want a stored record", err)
			}
			if d == nil || !reflect.DeepEqual(*d, property.Details{}) {
				t.Errorf("details = %+v, want a record with every field nil", d)
			}
			if string(raw) != body {
				t.Errorf("raw = %q, want the body verbatim", raw)
			}
			if calls.Load() != 1 {
				t.Errorf("calls=%d, want 1", calls.Load())
			}
		})
	}
}

func TestPropertyDetails_DecodeErrorIsNotTransient(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `<html>gateway</html>`,
		"data not object": `{"status":"OK","data":"nope"}`,
		// A record that names its zpid is this row's record even under an odd
		// status: failing to read it is about the row, not a provider outage.
		"named record under an odd status": `{"status":"PARTIAL","data":{"homeType":5,"zpid":"1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, calls := scripted(t, step{code: http.StatusOK, body: body})
			c, _ := newTestClient(t, srv.URL)

			_, _, err := c.PropertyDetails(context.Background(), "1", nil)
			if err == nil || errors.Is(err, ErrDetailsNotFound) {
				t.Fatalf("err = %v, want a decode error", err)
			}
			if IsTransient(err) {
				t.Errorf("a row-specific decode failure must count as an attempt: %v", err)
			}
			if calls.Load() != 1 {
				t.Errorf("calls=%d, want 1", calls.Load())
			}
		})
	}
}
