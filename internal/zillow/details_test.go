package zillow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
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
	d, raw, err := c.PropertyDetails(context.Background(), "43590635")
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
	d, _, err := c.PropertyDetails(context.Background(), "1")
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
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		c := New(srv.URL, "k", 5*time.Second)
		_, _, err := c.PropertyDetails(context.Background(), "gone")
		srv.Close()
		if !errors.Is(err, ErrDetailsNotFound) {
			t.Errorf("%s: err = %v, want ErrDetailsNotFound", name, err)
		}
	}
}

func TestPropertyDetails_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", 5*time.Second)
	_, _, err := c.PropertyDetails(context.Background(), "1")
	if err == nil || errors.Is(err, ErrDetailsNotFound) {
		t.Errorf("want a non-not-found error, got %v", err)
	}
}
