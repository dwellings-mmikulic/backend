package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

type feedTestAudience struct{}

func (feedTestAudience) LastChannel(context.Context, viewer.ID, time.Time) (string, bool, error) {
	return "", false, nil
}
func (feedTestAudience) Stats(context.Context, string, time.Time) (viewer.Stats, error) {
	return viewer.Stats{}, nil
}

func feedMux(store Store, now time.Time) (*Service, *http.ServeMux) {
	svc := testService(store, now)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableViewers(ViewerOptions{Hasher: viewer.NewHasher("salt", false), Audience: feedTestAudience{}, PublicBaseURL: "https://api.example.test"})
	h.EnableFeeds(FeedOptions{MobileBaseURL: "https://tv.example.test", QRShowSeconds: 60, QREverySeconds: 300})
	mux := http.NewServeMux()
	h.Register(mux)
	return svc, mux
}

func do(mux *http.ServeMux, method, path, ip string, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("X-Real-IP", ip)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

var hhRe = regexp.MustCompile(`live\.m3u8\?hh=([0-9a-f]{32})\n`)

func TestFeed_MasterIsPerHouseholdAndUncached(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != playlistContentType {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body)
	}
	a := hhRe.FindStringSubmatch(rec.Body.String())
	if a == nil {
		t.Fatalf("no household media URI:\n%s", rec.Body)
	}
	b := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())
	c := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.9", "").Body.String())
	if a[1] != b[1] || a[1] == c[1] {
		t.Errorf("same IP must share a feed and different IPs must not: %s %s %s", a[1], b[1], c[1])
	}
}

func TestFeed_Live(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	id := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())[1]
	rec := do(mux, "GET", "/feed/live.m3u8?hh="+id, "203.0.113.5", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "#EXTM3U\n") || rec.Header().Get("Cache-Control") != "public, max-age=2" {
		t.Errorf("status %d cc %q body %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
	}
	if rec := do(mux, "GET", "/feed/live.m3u8?hh="+strings.Repeat("0", 32), "203.0.113.5", ""); rec.Code != 404 {
		t.Errorf("unknown household: status %d body %s", rec.Code, rec.Body)
	}
	if rec := do(mux, "GET", "/feed/live.m3u8?hh=nope", "203.0.113.5", ""); rec.Code != 400 {
		t.Errorf("malformed hh: status %d", rec.Code)
	}
}

func TestFeed_QR(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/feed/qr.png", "203.0.113.5", "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 400 {
		t.Errorf("decode: %v, width %d", err, img.Bounds().Dx())
	}
	if rec := do(mux, "GET", "/feed/qr.png?size=50", "203.0.113.5", ""); rec.Code != 400 {
		t.Errorf("size 50: status %d", rec.Code)
	}
	// The QR's household must be the same one the master playlist uses.
	id := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())[1]
	var f map[string]any
	_ = json.Unmarshal(do(mux, "GET", "/feed/me", "203.0.113.5", "").Body.Bytes(), &f)
	if f["id"] != id {
		t.Errorf("/feed/me id %v != master's %s", f["id"], id)
	}
}

func TestFeed_GetAndChoose(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	m.addZip("77777")
	_, mux := feedMux(m, t0)
	var me struct {
		ID, Scope, Name, Requested, Source, Live string
	}
	rec := do(mux, "GET", "/feed/me", "203.0.113.5", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Scope != "us" || me.Source != SourceDefault || me.Name == "" || !strings.HasSuffix(me.Live, "/feed/live.m3u8?hh="+me.ID) {
		t.Fatalf("me = %+v", me)
	}
	rec = do(mux, "GET", "/feed/"+me.ID, "198.51.100.1", "") // any IP may read by id
	if rec.Code != 200 {
		t.Fatalf("get by id: %d", rec.Code)
	}

	rec = do(mux, "POST", "/feed/"+me.ID, "203.0.113.5", `{"zip":"78701"}`)
	var after struct{ Scope, Requested, Source string }
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if rec.Code != 200 || after.Scope != "zip:78701" || after.Source != SourceChoice {
		t.Fatalf("choose: %d %s", rec.Code, rec.Body)
	}
	rec = do(mux, "POST", "/feed/"+me.ID, "203.0.113.5", `{"zip":"77777"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if rec.Code != 200 || after.Scope != "us" || after.Requested != "zip:77777" {
		t.Errorf("thin zip must fall back and keep requested: %d %s", rec.Code, rec.Body)
	}
	for body, want := range map[string]int{
		`{"zip":"12345"}`: 404, // unknown area
		`{"zip":"1234"}`:  400,
		`{"city":"Katy"}`: 400, // city without state
		`not json`:        400,
		`{"zip":"` + strings.Repeat("7", 2000) + `"}`: 400, // over 1 KB
		`{}`: 200, // national: a valid choice
	} {
		if rec := do(mux, "POST", "/feed/"+me.ID, "203.0.113.5", body); rec.Code != want {
			t.Errorf("POST %.40s: status %d, want %d (%s)", body, rec.Code, want, rec.Body)
		}
	}
	if rec := do(mux, "POST", "/feed/"+strings.Repeat("0", 32), "203.0.113.5", `{"zip":"78701"}`); rec.Code != 404 {
		t.Errorf("unknown household: %d", rec.Code)
	}
	if rec := do(mux, "GET", "/feed/zz", "203.0.113.5", ""); rec.Code != 400 {
		t.Errorf("malformed id: %d", rec.Code)
	}
	rec = do(mux, "OPTIONS", "/feed/"+me.ID, "203.0.113.5", "")
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") || rec.Header().Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
}

// The page lives on dwellings.tv and calls api.dwellings.tv, so an error
// without the CORS header is a fetch rejection: the page could never show
// "We don't have that area yet" or "We could not find your TV".
func TestFeed_ErrorsCarryCORS(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	id := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())[1]
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/feed/live.m3u8?hh=" + strings.Repeat("0", 32), "", 404},
		{"GET", "/feed/" + strings.Repeat("0", 32), "", 404},
		{"POST", "/feed/" + id, `{"zip":"12345"}`, 404},
		{"POST", "/feed/" + id, `{"zip":"1234"}`, 400},
	} {
		rec := do(mux, c.method, c.path, "203.0.113.5", c.body)
		if rec.Code != c.want || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s %s: status %d (want %d), ACAO %q", c.method, c.path, rec.Code, c.want, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestFeed_ChoiceBodyTooLargeNamesTheProblem(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	id := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())[1]
	rec := do(mux, "POST", "/feed/"+id, "203.0.113.5", `{"zip":"`+strings.Repeat("7", 2000)+`"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "1 KB") {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
}

// The household is the connection's address only. Honouring a public ip=
// parameter would let anyone who knows a home's address read its feed id
// (and so change its TV) — the property the spec promises the id has.
func TestFeed_HouseholdIgnoresIPParam(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	var me, spoofed map[string]any
	_ = json.Unmarshal(do(mux, "GET", "/feed/me", "203.0.113.5", "").Body.Bytes(), &me)
	_ = json.Unmarshal(do(mux, "GET", "/feed/me?ip=203.0.113.9", "203.0.113.5", "").Body.Bytes(), &spoofed)
	if me["id"] != spoofed["id"] {
		t.Errorf("/feed/me honoured ip=: %v vs %v", me["id"], spoofed["id"])
	}
	master := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8?ip=203.0.113.9", "203.0.113.5", "").Body.String())
	if master == nil || master[1] != me["id"] {
		t.Errorf("/feed/master.m3u8 honoured ip=: %v vs %v", master, me["id"])
	}
}

func TestFeed_Areas(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/feed/areas", "203.0.113.5", "")
	var areas []struct {
		City, State, Name string
		Clips             int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &areas); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(areas) != 1 || areas[0].Name != "Katy, TX" || areas[0].Clips != 40 || areas[0].City != "katy" {
		t.Errorf("areas = %+v", areas)
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=600" {
		t.Errorf("cache control %q", rec.Header().Get("Cache-Control"))
	}
}

func TestFeed_ResolveAdvertisesFeedAndQR(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/channels/resolve", "203.0.113.5", "")
	var res struct {
		Feed string
		QR   struct {
			URL          string
			ShowSeconds  int `json:"show_seconds"`
			EverySeconds int `json:"every_seconds"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if res.Feed != "https://api.example.test/feed/master.m3u8" || res.QR.URL != "https://api.example.test/feed/qr.png" || res.QR.ShowSeconds != 60 || res.QR.EverySeconds != 300 {
		t.Errorf("resolve = %+v", res)
	}
}

func TestFeed_NotMountedWithoutEnableFeeds(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/feed/master.m3u8")
	if rec.Code != 404 {
		t.Errorf("status %d", rec.Code)
	}
}

func TestFeed_TVPage(t *testing.T) {
	_, mux := feedMux(newMemStore(), t0)
	for _, p := range []string{"/tv/", "/tv/" + strings.Repeat("a", 32)} {
		rec := do(mux, "GET", p, "203.0.113.5", "")
		body := rec.Body.String()
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(body, `"https://api.example.test"`) || strings.Contains(body, "__API_BASE__") {
			t.Errorf("%s: API base not injected", p)
		}
		if !strings.Contains(body, "hls.js/1.5.13/hls.min.js") || !strings.Contains(body, "/feed/areas") {
			t.Errorf("%s: page is missing the player or the areas call", p)
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: cache control %q", p, rec.Header().Get("Cache-Control"))
		}
	}
}

// Chrome on macOS answers "maybe" to canPlayType for HLS and then fails to
// demux it, so the page must try hls.js first and fall back to the native
// player only where hls.js cannot run (Safari on iOS).
func TestFeed_TVPagePrefersHlsJSOverNativePlayback(t *testing.T) {
	_, mux := feedMux(newMemStore(), t0)
	body := do(mux, "GET", "/tv/", "203.0.113.5", "").Body.String()
	hls, native := strings.Index(body, "Hls.isSupported()"), strings.Index(body, "canPlayType(")
	if hls < 0 || native < 0 || hls > native {
		t.Errorf("play() must check Hls.isSupported() (at %d) before canPlayType (at %d)", hls, native)
	}
}
