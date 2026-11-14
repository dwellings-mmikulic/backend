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
func (feedTestAudience) Stats(context.Context, string, string, time.Time) (viewer.Stats, error) {
	return viewer.Stats{}, nil
}

func feedMux(store Store, now time.Time) (*Service, *http.ServeMux) {
	svc := testService(store, now)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableViewers(ViewerOptions{Hasher: platformHasher(), Audience: feedTestAudience{}, PublicBaseURL: "https://api.example.test"})
	h.EnableFeeds(FeedOptions{MobileBaseURL: "https://tv.example.test", QRShowSeconds: 60, QREverySeconds: 300})
	mux := http.NewServeMux()
	h.Register(mux)
	return svc, mux
}

// platformAddr is the address of a platform's server in these tests: the
// one connecting address whose ip= parameter is believed.
const platformAddr = "198.51.100.1"

// pageOrigin is the origin of the mobile page (feedMux's MobileBaseURL).
const pageOrigin = "https://tv.example.test"

func platformHasher() *viewer.Hasher {
	nets, err := viewer.ParseCIDRs(platformAddr)
	if err != nil {
		panic(err)
	}
	h := viewer.NewHasher("salt", false)
	h.TrustIPParamFrom(nets)
	return h
}

// do sends a request with no Origin header, as the TV app does.
func do(mux *http.ServeMux, method, path, ip string, body string) *httptest.ResponseRecorder {
	return doFrom(mux, method, path, ip, "", body)
}

// doFrom is do as a browser page on origin would send it.
func doFrom(mux *http.ServeMux, method, path, ip, origin string, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("X-Real-IP", ip)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
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
	rec := doFrom(mux, "GET", "/feed/me", "203.0.113.5", pageOrigin, "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != pageOrigin {
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
		rec := doFrom(mux, c.method, c.path, "203.0.113.5", pageOrigin, c.body)
		// The personal playlist is public to whoever holds its id, like the
		// channels. The routes that speak for a home name the page's origin.
		wantACAO := pageOrigin
		if strings.HasPrefix(c.path, "/feed/live.m3u8") {
			wantACAO = "*"
		}
		if rec.Code != c.want || rec.Header().Get("Access-Control-Allow-Origin") != wantACAO {
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
		if !strings.Contains(body, "/feed/areas") {
			t.Errorf("%s: page is missing the areas call", p)
		}
		// Marko, 2026-10-21: the dwellings.tv logo top left (inlined, so the
		// page is whole on any origin) and the same-Wi-Fi notice at the bottom.
		if !strings.Contains(body, `class="logo"`) || !strings.Contains(body, "<svg") || !strings.Contains(body, "Dwellings<span>.TV</span>") {
			t.Errorf("%s: page is missing the logo", p)
		}
		// Daniel, 2026-10-21: the intro line and the button label.
		if !strings.Contains(body, "Enter a zip code or city to customize your stream.") || !strings.Contains(body, ">Apply Location<") {
			t.Errorf("%s: page is missing Daniel's copy", p)
		}
		if !strings.Contains(body, "Your mobile device must be connected to the same Wi-Fi network as your TV to customize the channel.") {
			t.Errorf("%s: page is missing the same-Wi-Fi notice", p)
		}
		// Marko, 2026-10-21: a quiet share row for the channel itself. It
		// shares the public site, never the household's /tv/ link, because
		// that link controls their TV.
		if !strings.Contains(body, `id="share"`) || !strings.Contains(body, "navigator.share") || !strings.Contains(body, `SHARE_URL = "https://dwellings.tv/"`) {
			t.Errorf("%s: page is missing the share row", p)
		}
		// Daniel, 2026-10-15: no in-page player and no "watch here" copy;
		// platforms will not carry a channel that pulls viewers to our site.
		if strings.Contains(body, "No TV nearby") || strings.Contains(body, "hls.min.js") || strings.Contains(body, "<video") {
			t.Errorf("%s: page still carries the in-page player", p)
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: cache control %q", p, rec.Header().Get("Cache-Control"))
		}
	}
}

// Platforms list the channel in their guide; the description tells viewers
// where to customize it (Daniel, 2026-10-15).
func TestFeed_EPGDescriptionsCarryTheCustomizeURL(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/channels/epg.json?zip=77494", "203.0.113.5", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var e EPG
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Programs) == 0 {
		t.Fatal("no programs")
	}
	for _, p := range e.Programs {
		if !strings.HasSuffix(p.Description, " · Customize this channel at tv.example.test/tv") {
			t.Errorf("description %q lacks the customize URL", p.Description)
		}
		if strings.Contains(p.Description, "https://") {
			t.Errorf("description %q should show the bare host, not the scheme", p.Description)
		}
	}
}

// /feed/me hands out the feed id of the network the browser is on, and the
// id is all it takes to change that TV's feed. Only the mobile page may read
// it from a browser, and only it may post a choice.
func TestFeed_OnlyTheMobilePageReadsAHomesFeedFromABrowser(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	_, mux := feedMux(m, t0)
	home := "203.0.113.5"
	id := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", home, "").Body.String())[1]

	for _, path := range []string{"/feed/me", "/feed/master.m3u8", "/feed/qr.png", "/feed/" + id} {
		for origin, want := range map[string]string{
			pageOrigin:                        pageOrigin,
			"https://www.tv.example.test":     "https://www.tv.example.test",
			"https://evil.example":            "",
			"https://tv.example.test.evil.io": "",
			"http://tv.example.test":          "",
			"null":                            "",
			"":                                "",
		} {
			rec := doFrom(mux, "GET", path, home, origin, "")
			if rec.Code != 200 {
				t.Fatalf("GET %s from %q: status %d", path, origin, rec.Code)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != want {
				t.Errorf("GET %s from %q: Access-Control-Allow-Origin %q, want %q", path, origin, got, want)
			}
			if rec.Header().Get("Vary") != "Origin" {
				t.Errorf("GET %s from %q: Vary %q", path, origin, rec.Header().Get("Vary"))
			}
		}
	}

	// A page elsewhere cannot post a choice, whatever content type it uses.
	body := `{"city":"austin","state":"tx"}`
	req := httptest.NewRequest("POST", "/feed/"+id, strings.NewReader(body))
	req.Header.Set("X-Real-IP", "198.51.100.77")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("POST from another origin: status %d, ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	var f struct{ Scope string }
	_ = json.Unmarshal(do(mux, "GET", "/feed/"+id, home, "").Body.Bytes(), &f)
	if f.Scope == "city:austin:tx" {
		t.Fatal("the refused post changed the home's feed")
	}
	// The page itself, the TV app (no Origin) and a same-origin page still can.
	if rec := doFrom(mux, "POST", "/feed/"+id, home, pageOrigin, body); rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != pageOrigin {
		t.Errorf("POST from the page: status %d, ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec := do(mux, "POST", "/feed/"+id, home, `{"zip":"77494"}`); rec.Code != 200 {
		t.Errorf("POST without Origin: status %d", rec.Code)
	}
	if rec := doFrom(mux, "POST", "/feed/"+id, home, "http://example.com", body); rec.Code != 200 {
		t.Errorf("same-origin POST (httptest host is example.com): status %d", rec.Code)
	}
	// The preflight answers the page and nobody else.
	if rec := doFrom(mux, "OPTIONS", "/feed/"+id, home, pageOrigin, ""); rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != pageOrigin || rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("preflight from the page: %d %v", rec.Code, rec.Header())
	}
	if rec := doFrom(mux, "OPTIONS", "/feed/"+id, home, "https://evil.example", ""); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight from another origin was answered: %v", rec.Header())
	}
}

func TestFeed_ExtraAllowedOrigins(t *testing.T) {
	o := FeedOptions{MobileBaseURL: "https://www.dwellings.tv", AllowedOrigins: []string{"https://app.example/"}}
	for origin, want := range map[string]bool{
		"https://dwellings.tv": true, "https://www.dwellings.tv": true, "https://app.example": true,
		"https://dwellings.tv.evil.io": false, "http://dwellings.tv": false, "": false, "*": false,
	} {
		if got := o.allowsOrigin(origin); got != want {
			t.Errorf("allowsOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

// ip= names whose feed is served and whose views are recorded. From an
// address that is not a platform's it must change nothing.
func TestHandler_IPParamFromAStrangerIsIgnored(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	svc, mux := feedMux(m, t0)
	home, stranger := "203.0.113.7", "198.51.100.200"
	if rec := do(mux, "GET", "/feed/me", home, ""); rec.Code != 200 {
		t.Fatalf("/feed/me: %d", rec.Code)
	}
	id := viewer.NewHasher("salt", false).Household(home)
	if _, err := svc.Choose(context.Background(), id, katy, t0); err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return t0.Add(5 * time.Minute) }
	rec := do(mux, "GET", "/channels/live.m3u8?zip=78701&ip="+home, stranger, "")
	plain := do(mux, "GET", "/channels/live.m3u8?zip=78701", stranger, "")
	if rec.Code != 200 || rec.Body.String() != plain.Body.String() {
		t.Fatalf("a stranger's ip= changed the playlist:\n%s\n---\n%s", rec.Body.String(), plain.Body.String())
	}
	if a, _ := m.Anchor(context.Background(), id, "zip:78701"); a != nil {
		t.Fatalf("a stranger's ip= created an anchor for the home: %+v", a)
	}
	// The master does not hand the claimed address on either.
	if body := do(mux, "GET", "/channels/master.m3u8?zip=78701&sid=tv-1&ip="+home, stranger, "").Body.String(); !strings.HasSuffix(body, "\nlive.m3u8?sid=tv-1&zip=78701\n") {
		t.Errorf("master carried a stranger's ip=:\n%s", body)
	}
	if body := do(mux, "GET", "/channels/master.m3u8?zip=78701&sid=tv-1&ip="+home, platformAddr, "").Body.String(); !strings.HasSuffix(body, "\nlive.m3u8?ip="+home+"&sid=tv-1&zip=78701\n") {
		t.Errorf("master dropped a platform's ip=:\n%s", body)
	}
}

func TestHandler_LiveWithIPFollowsTheHousehold(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	svc, mux := feedMux(m, t0)
	ip := "203.0.113.7"
	// Before the phone opens the page: the channel, as advertised, and no
	// anchor is created.
	svc.now = func() time.Time { return t0.Add(time.Minute) }
	rec := do(mux, "GET", "/channels/live.m3u8?zip=78701&ip="+ip, "198.51.100.1", "")
	plain := do(mux, "GET", "/channels/live.m3u8?zip=78701", "198.51.100.1", "")
	if rec.Code != 200 || rec.Body.String() != plain.Body.String() {
		t.Fatalf("status %d; ip= changed the channel before any household existed:\n%s\n---\n%s", rec.Code, rec.Body.String(), plain.Body.String())
	}
	if a, _ := m.Anchor(context.Background(), viewer.NewHasher("salt", false).Household(ip), "zip:78701"); a != nil {
		t.Fatalf("anchor created for a household that does not exist: %+v", a)
	}
	// The phone (same public IP) opens the page: household created on katy.
	if rec := do(mux, "GET", "/feed/me", ip, ""); rec.Code != 200 {
		t.Fatalf("/feed/me: %d %s", rec.Code, rec.Body.String())
	}
	// The platform's next poll anchors the household to the channel…
	svc.now = func() time.Time { return t0.Add(2 * time.Minute) }
	rec = do(mux, "GET", "/channels/live.m3u8?zip=78701&ip="+ip, "198.51.100.1", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != playlistContentType {
		t.Errorf("content type %q", ct)
	}
	// …and three minutes later the channel route with ip= lists katy only.
	svc.now = func() time.Time { return t0.Add(5 * time.Minute) }
	rec = do(mux, "GET", "/channels/live.m3u8?zip=78701&ip="+ip, "198.51.100.1", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	for _, u := range parsePlaylist(t, rec.Body.Bytes()).urls {
		if clipID(t, u) > 100 {
			t.Fatalf("austin listed on the household's feed:\n%s", rec.Body.String())
		}
	}
	// Without ip= the channel is untouched.
	plain = do(mux, "GET", "/channels/live.m3u8?zip=78701", "198.51.100.1", "")
	for _, u := range parsePlaylist(t, plain.Body.Bytes()).urls {
		if clipID(t, u) <= 100 {
			t.Fatalf("katy listed on the plain channel:\n%s", plain.Body.String())
		}
	}
}

func TestHandler_LiveIgnoresNonPublicIP(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	svc, mux := feedMux(m, t0)
	if rec := do(mux, "GET", "/feed/me", "203.0.113.7", ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	svc.now = func() time.Time { return t0.Add(4 * time.Minute) }
	plain := do(mux, "GET", "/channels/live.m3u8?zip=78701", "198.51.100.1", "").Body.String()
	for _, ip := range []string{"10.0.0.5", "100.64.1.1", "127.0.0.1", "not-an-ip", ""} {
		rec := do(mux, "GET", "/channels/live.m3u8?zip=78701&ip="+ip, "203.0.113.7", "")
		if rec.Code != 200 || rec.Body.String() != plain {
			t.Errorf("ip=%q: status %d, body differs from the plain channel", ip, rec.Code)
		}
	}
}

func TestHandler_LiveWithIPWithoutFeedsIsTheChannel(t *testing.T) {
	m := newMemStore()
	addClips(m, austin, 101, 40)
	rec := serve(t, m, t0, "/channels/live.m3u8?zip=78701&ip=203.0.113.7")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
