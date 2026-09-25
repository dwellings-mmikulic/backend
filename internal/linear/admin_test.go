package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

type fakeClients struct {
	rows  []viewer.ClientSeen
	since time.Time
	limit int
}

func (f *fakeClients) Clients(_ context.Context, since time.Time, limit int) ([]viewer.ClientSeen, error) {
	f.since, f.limit = since, limit
	return f.rows, nil
}

func adminOpts(fc *fakeClients) ViewerOptions {
	o := baseOpts(&fakeAudience{}, nil)
	o.Clients, o.AdminKey = fc, "k3y"
	return o
}

func TestAdminViewers(t *testing.T) {
	fc := &fakeClients{rows: []viewer.ClientSeen{{IP: "99.178.140.144", UserAgent: "Roku/DVP-15.3", Channel: "us", FirstSeen: t0, LastSeen: t0}}}
	rec := serveViewers(t, newMemStore(), adminOpts(fc), "/admin/viewers?key=k3y&hours=2&limit=10", "203.0.113.5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("headers: %v", rec.Header())
	}
	if !fc.since.Equal(t0.Add(-2*time.Hour)) || fc.limit != 10 {
		t.Errorf("since %v limit %d", fc.since, fc.limit)
	}
	var body struct {
		Count   int                 `json:"count"`
		Viewers []viewer.ClientSeen `json:"viewers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || body.Viewers[0].IP != "99.178.140.144" {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestAdminViewers_Auth(t *testing.T) {
	fc := &fakeClients{}
	for _, tc := range []struct {
		name, path, auth string
		want             int
	}{
		{"no key", "/admin/viewers", "", http.StatusUnauthorized},
		{"wrong key", "/admin/viewers?key=nope", "", http.StatusUnauthorized},
		{"wrong bearer beats right query key", "/admin/viewers?key=k3y", "Bearer nope", http.StatusUnauthorized},
		{"bearer", "/admin/viewers", "Bearer k3y", http.StatusOK},
		{"bad hours", "/admin/viewers?key=k3y&hours=0", "", http.StatusBadRequest},
		{"bad limit", "/admin/viewers?key=k3y&limit=x", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveViewersAuth(t, adminOpts(fc), tc.path, tc.auth)
			if rec.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// An empty listing is [], not null.
	rec := serveViewersAuth(t, adminOpts(fc), "/admin/viewers?key=k3y", "")
	var body map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if string(body["viewers"]) != "[]" {
		t.Errorf("empty viewers = %s", body["viewers"])
	}
}

func TestAdminViewers_UnmountedWithoutKey(t *testing.T) {
	o := adminOpts(&fakeClients{})
	o.AdminKey = ""
	if rec := serveViewers(t, newMemStore(), o, "/admin/viewers", "203.0.113.5"); rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}

func TestTrack_RecordsPublicClientIP(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	tr := &fakeTracker{}
	o := baseOpts(&fakeAudience{}, nil)
	o.Tracker = tr
	// Roku fills ip= with its LAN address; the public address the request
	// came from is the one kept.
	serveViewers(t, m, o, "/channels/live.m3u8?ip=10.0.0.90", "99.178.140.144")
	if len(tr.beats) != 1 || tr.beats[0].Client.IP != "99.178.140.144" {
		t.Fatalf("beats = %+v", tr.beats)
	}
	if tr.beats[0].Viewer != viewer.NewHasher("s", false).IDAt("", "99.178.140.144", t0) {
		t.Error("viewer must be identified by the public client IP")
	}
}
