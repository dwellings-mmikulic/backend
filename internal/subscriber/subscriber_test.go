package subscriber

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore struct {
	email string
	meta  json.RawMessage
	calls int
	err   error
}

func (f *fakeStore) Upsert(_ context.Context, email string, meta json.RawMessage) error {
	f.calls++
	f.email, f.meta = email, meta
	return f.err
}

func serve(t *testing.T, st Store, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	NewHandler(st, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/api/v1/platform_subscriber", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestSubscribe(t *testing.T) {
	st := &fakeStore{}
	rec := serve(t, st, http.MethodPut, `{"email":"  Viewer@Example.COM ","metadata":{"device":"roku","zip":"77494"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec)["message"]; got != "Platform subscriber updated successfully" {
		t.Errorf("message = %v", got)
	}
	if st.email != "viewer@example.com" {
		t.Errorf("email stored as %q, want normalised", st.email)
	}
	if string(st.meta) != `{"device":"roku","zip":"77494"}` {
		t.Errorf("metadata = %s", st.meta)
	}
	for _, h := range [][2]string{
		{"Access-Control-Allow-Origin", "*"},
		{"Content-Type", "application/json"},
		{"Cache-Control", "no-store"},
	} {
		if got := rec.Header().Get(h[0]); got != h[1] {
			t.Errorf("%s = %q, want %q", h[0], got, h[1])
		}
	}
}

func TestSubscribe_MetadataOptional(t *testing.T) {
	for _, body := range []string{`{"email":"a@b.co"}`, `{"email":"a@b.co","metadata":null}`} {
		st := &fakeStore{}
		if rec := serve(t, st, http.MethodPut, body); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", body, rec.Code, rec.Body.String())
		}
		if st.meta != nil {
			t.Errorf("%s: metadata = %s, want nil", body, st.meta)
		}
	}
}

func TestSubscribe_Validation(t *testing.T) {
	cases := map[string]struct {
		body  string
		field string
	}{
		"missing email": {`{"metadata":{}}`, "email"},
		"blank email":   {`{"email":"   "}`, "email"},
		"no domain dot": {`{"email":"viewer@localhost"}`, "email"},
		"display name":  {`{"email":"Viewer <v@example.com>"}`, "email"},
		"not an email":  {`{"email":"viewer"}`, "email"},
		"not json":      {`{"email":`, "body"},
		"array body":    {`[1,2]`, "body"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			rec := serve(t, st, http.MethodPut, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			errs, _ := decode(t, rec)["errors"].(map[string]any)
			if _, ok := errs[c.field]; !ok {
				t.Errorf("errors = %v, want key %q", errs, c.field)
			}
			if st.calls != 0 {
				t.Error("store called on invalid input")
			}
		})
	}
}

func TestSubscribe_StoreError(t *testing.T) {
	rec := serve(t, &fakeStore{err: errors.New("boom")}, http.MethodPut, `{"email":"a@b.co"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if got := decode(t, rec)["error"]; got != "Failed to subscribe" {
		t.Errorf("error = %v", got)
	}
}

func TestSubscribe_TooLarge(t *testing.T) {
	body := `{"email":"a@b.co","metadata":{"blob":"` + strings.Repeat("x", maxBodyBytes) + `"}}`
	if rec := serve(t, &fakeStore{}, http.MethodPut, body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestPreflight(t *testing.T) {
	rec := serve(t, &fakeStore{}, http.MethodOptions, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "PUT") ||
		rec.Header().Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Errorf("headers: %v", rec.Header())
	}
}

func TestOtherMethods(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if rec := serve(t, &fakeStore{}, m, `{"email":"a@b.co"}`); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d", m, rec.Code)
		}
	}
}
