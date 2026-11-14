package linear

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dwellingtw/backend/internal/music"
	"github.com/dwellingtw/backend/internal/music/musictest"
)

// musicMux is the channel routes with the music enabled over cdn.
func musicMux(t *testing.T, store Store, cdn string, byDefault bool) *http.ServeMux {
	t.Helper()
	loop, err := music.Open(musictest.WriteLoop(t, 2000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })
	h := NewHandler(testService(store, t0), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableMusic(MusicOptions{Loop: loop, Default: byDefault, CDNBaseURL: cdn})
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func get(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func segmentLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func TestMusic_PlaylistListsSegmentsThroughTheMusicRoute(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	mux := musicMux(t, m, "https://cdn", false)

	plain := get(mux, "/channels/live.m3u8?zip=77494").Body.String()
	with := get(mux, "/channels/live.m3u8?zip=77494&music=1").Body.String()
	a, b := segmentLines(plain), segmentLines(with)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("%d segments without music, %d with", len(a), len(b))
	}
	for i := range a {
		if !strings.HasPrefix(a[i], "https://cdn/hls/") {
			t.Fatalf("without music, segment %q is not on the CDN", a[i])
		}
		rest, ok := strings.CutPrefix(b[i], "/channels/m/")
		if !ok {
			t.Fatalf("with music, segment %q is not on the music route", b[i])
		}
		spec, path, _ := strings.Cut(rest, "/")
		if "https://cdn/"+path != a[i] || strings.Count(spec, "-") != 2 {
			t.Fatalf("segment %q does not name %q", b[i], a[i])
		}
	}
	// Tags are untouched: only the segment lines differ.
	tags := func(s string) string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if strings.HasPrefix(l, "#") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if tags(plain) != tags(with) {
		t.Error("the music changed the playlist's tags")
	}
}

func TestMusic_SegmentURLCarriesAiringTimeDurationAndOffset(t *testing.T) {
	segs := []segment{
		{URL: "https://cdn/hls/v1/7/abc/seg-002.ts", DurMS: 6667, Start: t0, ElapsedMS: 10000},
		{URL: "https://elsewhere/hls/v1/7/abc/seg-003.ts", DurMS: 5000, Start: t0},
		{URL: "https://cdn/videos/7.mp4", DurMS: 5000, Start: t0},
	}
	out := musicSegments(segs, "https://cdn")
	if want := "/channels/m/1787659200000-6667-10000/hls/v1/7/abc/seg-002.ts"; out[0].URL != want {
		t.Errorf("url = %q, want %q", out[0].URL, want)
	}
	if out[1].URL != segs[1].URL || out[2].URL != segs[2].URL {
		t.Error("a segment that is not an HLS segment on the CDN was rewritten")
	}
	if segs[0].URL != "https://cdn/hls/v1/7/abc/seg-002.ts" {
		t.Error("the input was modified")
	}
}

func TestMusic_DefaultAndOverride(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	on := func(mux *http.ServeMux, path string) bool {
		return strings.Contains(get(mux, path).Body.String(), "/channels/m/")
	}
	off := musicMux(t, m, "https://cdn", false)
	if on(off, "/channels/live.m3u8?zip=77494") || !on(off, "/channels/live.m3u8?zip=77494&music=1") {
		t.Error("default off: music must come only with music=1")
	}
	dflt := musicMux(t, m, "https://cdn", true)
	if !on(dflt, "/channels/live.m3u8?zip=77494") || on(dflt, "/channels/live.m3u8?zip=77494&music=0") {
		t.Error("default on: music must come unless music=0")
	}
	if on(dflt, "/channels/live.m3u8?zip=77494&music=yes") != true {
		t.Error("an unknown music value must fall back to the default")
	}
}

func TestMusic_PlatformPathCarriesTheMusic(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	loop, err := music.Open(musictest.WriteLoop(t, 2000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })
	h := NewHandler(testService(m, t0), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableMusic(MusicOptions{Loop: loop, Platforms: []string{"aeibehce"}, CDNBaseURL: "https://cdn"})
	mux := http.NewServeMux()
	h.Register(mux)
	for path, want := range map[string]bool{
		"/channels/aeibehce/live.m3u8?zip=77494":         true,
		"/channels/aeibehce/live.m3u8?zip=77494&music=0": false,
		"/channels/cineplex/live.m3u8?zip=77494":         false,
		"/channels/live.m3u8?zip=77494":                  false,
	} {
		rec := get(mux, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body.String())
		}
		if got := strings.Contains(rec.Body.String(), "/channels/m/"); got != want {
			t.Errorf("%s: music = %v, want %v", path, got, want)
		}
	}
}

func TestMusic_MasterHandsTheParameterOn(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	mux := musicMux(t, m, "https://cdn", false)
	for path, want := range map[string]string{
		"/channels/master.m3u8?zip=77494&music=1":   "live.m3u8?music=1&zip=77494\n",
		"/channels/master.m3u8?zip=77494&music=0":   "live.m3u8?music=0&zip=77494\n",
		"/channels/master.m3u8?zip=77494":           "live.m3u8?zip=77494\n",
		"/channels/master.m3u8?zip=77494&music=abc": "live.m3u8?zip=77494\n",
	} {
		if body := get(mux, path).Body.String(); !strings.HasSuffix(body, want) {
			t.Errorf("%s: master ends %q, want %q", path, body[strings.LastIndex(strings.TrimSpace(body), "\n")+1:], want)
		}
	}
}

func TestMusic_WithoutMusicEnabledNothingChanges(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	rec := serve(t, m, t0, "/channels/live.m3u8?zip=77494&music=1")
	if strings.Contains(rec.Body.String(), "/channels/m/") {
		t.Error("music listed although it is not enabled")
	}
	if rec := serve(t, m, t0, "/channels/m/1-1000-0/hls/v1/1/x/seg-000.ts"); rec.Code != http.StatusNotFound {
		t.Errorf("music route answers %d although it is not enabled", rec.Code)
	}
}

func TestMusic_SegmentRoute(t *testing.T) {
	good := musictest.Segment(132000, 60, true)
	silent := musictest.Segment(132000, 60, false)
	var asked []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/hls/v1/1/x/seg-000.ts":
			w.Write(good)
		case "/hls/v1/2/x/seg-000.ts":
			w.Write(silent)
		case "/hls/v1/3/x/seg-000.ts":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cdn.Close()
	mux := musicMux(t, newMemStore(), cdn.URL, true)

	t.Run("swaps the sound", func(t *testing.T) {
		rec := get(mux, "/channels/m/1787659200000-2000-0/hls/v1/1/x/seg-000.ts")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.Bytes()
		if bytes.Equal(body, good) || bytes.Contains(body, []byte("the clip's own sound")) {
			t.Error("the clip's own sound is still in the segment")
		}
		if !bytes.Contains(body, musictest.Frame(int(1787659200000 * musictest.SampleRate / 1024000 % 2000))[:60]) {
			t.Error("the loop frame on air is not in the segment")
		}
		if ct, cc := rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"); ct != "video/mp2t" || cc != "public, max-age=600" {
			t.Errorf("content type %q, cache control %q", ct, cc)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Error("missing CORS header")
		}
	})
	t.Run("serves a segment without a sound track as it is", func(t *testing.T) {
		rec := get(mux, "/channels/m/1787659200000-2000-0/hls/v1/2/x/seg-000.ts")
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), silent) {
			t.Errorf("status %d, body changed: %v", rec.Code, !bytes.Equal(rec.Body.Bytes(), silent))
		}
	})
	t.Run("serves the segment as it is when the timing is nonsense", func(t *testing.T) {
		rec := get(mux, "/channels/m/5-2000-9000/hls/v1/1/x/seg-000.ts")
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), good) {
			t.Errorf("status %d, body changed: %v", rec.Code, !bytes.Equal(rec.Body.Bytes(), good))
		}
	})
	t.Run("passes a missing segment on as 404 and a CDN failure as 502", func(t *testing.T) {
		if rec := get(mux, "/channels/m/1787659200000-2000-0/hls/v1/9/x/seg-000.ts"); rec.Code != http.StatusNotFound {
			t.Errorf("missing segment: status %d", rec.Code)
		}
		if rec := get(mux, "/channels/m/1787659200000-2000-0/hls/v1/3/x/seg-000.ts"); rec.Code != http.StatusBadGateway {
			t.Errorf("CDN failure: status %d", rec.Code)
		}
	})
	t.Run("fetches nothing but HLS segments", func(t *testing.T) {
		asked = nil
		for _, path := range []string{
			"/channels/m/1-2000-0/videos/1.mp4",
			"/channels/m/1-2000-0/hls/v1/1/x/index.m3u8",
			"/channels/m/1-2000-0/hls/v1/1/%2e%2e/seg-000.ts",
			"/channels/m/1-2000-0/hls/v1/1/x/seg-000.ts%3Fa=b",
			"/channels/m/1-2000/hls/v1/1/x/seg-000.ts",
			"/channels/m/a-b-c/hls/v1/1/x/seg-000.ts",
			"/channels/m/1-2000-0/",
		} {
			if rec := get(mux, path); rec.Code != http.StatusNotFound {
				t.Errorf("%s: status %d, want 404", path, rec.Code)
			}
		}
		if len(asked) != 0 {
			t.Errorf("the CDN was asked for %v", asked)
		}
	})
}
