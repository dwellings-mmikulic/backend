package geo

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeAPI(t *testing.T, calls *int32, handler func(ip string) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		const prefix = "/api/v1/json/"
		if len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		code, body := handler(r.URL.Path[len(prefix):])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const usBody = `{"ipVersion":4,"ipAddress":"8.8.8.8","countryCode":"US","zipCode":"94035","cityName":"Mountain View","regionName":"California","regionCode":"CA","isProxy":false}`

func TestFreeIPAPI_LocatesUSAddresses(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(ip string) (int, string) {
		if ip != "8.8.8.8" {
			t.Errorf("asked for %s", ip)
		}
		return 200, usBody
	})
	c := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	loc, ok := c.Locate(net.ParseIP("8.8.8.8"))
	if !ok || loc != (Location{Zip: "94035", City: "Mountain View", State: "ca"}) {
		t.Fatalf("got %+v %v", loc, ok)
	}
}

func TestFreeIPAPI_NonUSIsAMiss(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) {
		return 200, `{"countryCode":"AU","zipCode":"4000","cityName":"Sydney","regionCode":"NSW"}`
	})
	if loc, ok := NewFreeIPAPI(srv.URL+"/api/v1", nil).Locate(net.ParseIP("1.1.1.1")); ok || loc != (Location{}) {
		t.Fatalf("got %+v %v", loc, ok)
	}
}

// The free API answers a private address with nulls and a "-" zip; it is not
// worth a request, and the dash must never reach the resolver.
func TestFreeIPAPI_PrivateAndNilAddressesAreNotLookedUp(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { return 200, `{"zipCode":"-"}` })
	c := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	for _, ip := range []net.IP{nil, net.ParseIP("10.0.0.1"), net.ParseIP("192.168.1.9"), net.ParseIP("127.0.0.1"), net.ParseIP("::1")} {
		if _, ok := c.Locate(ip); ok {
			t.Errorf("%v located", ip)
		}
	}
	if calls != 0 {
		t.Errorf("%d requests for private addresses", calls)
	}
}

func TestFreeIPAPI_DashFieldsAreEmpty(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) {
		return 200, `{"countryCode":"US","zipCode":"-","cityName":"-","regionCode":"TX"}`
	})
	loc, ok := NewFreeIPAPI(srv.URL+"/api/v1", nil).Locate(net.ParseIP("8.8.4.4"))
	if !ok || loc != (Location{State: "tx"}) {
		t.Fatalf("got %+v %v", loc, ok)
	}
}

// Playlists poll every few seconds; one request per address per day is the
// most the free tier can bear.
func TestFreeIPAPI_CachesPerAddress(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { return 200, usBody })
	c := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Locate(net.ParseIP("8.8.8.8")) }()
	}
	wg.Wait()
	c.Locate(net.ParseIP("8.8.8.8"))
	if calls != 1 {
		t.Errorf("%d requests for one address", calls)
	}
	c.Locate(net.ParseIP("8.8.4.4"))
	if calls != 2 {
		t.Errorf("%d requests for two addresses", calls)
	}
}

// A failure (rate limit, outage) is a miss, remembered briefly so a burst of
// polls from one new address does not hammer the API, then retried.
func TestFreeIPAPI_FailuresAreMissesRetriedLater(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { return 429, `{"error":"rate limit"}` })
	c := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, ok := c.Locate(net.ParseIP("8.8.8.8")); ok {
			t.Fatal("located on 429")
		}
	}
	if calls != 1 {
		t.Errorf("%d requests during the failure window", calls)
	}
	now = now.Add(failureTTL + time.Second)
	c.Locate(net.ParseIP("8.8.8.8"))
	if calls != 2 {
		t.Errorf("%d requests after the failure window", calls)
	}
}

func TestFreeIPAPI_HitsExpire(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { return 200, usBody })
	c := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	c.Locate(net.ParseIP("8.8.8.8"))
	now = now.Add(hitTTL + time.Second)
	c.Locate(net.ParseIP("8.8.8.8"))
	if calls != 2 {
		t.Errorf("%d requests across the TTL", calls)
	}
}

func TestFreeIPAPI_SlowAPIIsAMiss(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { time.Sleep(300 * time.Millisecond); return 200, usBody })
	c := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	c.http.Timeout = 50 * time.Millisecond
	start := time.Now()
	if _, ok := c.Locate(net.ParseIP("8.8.8.8")); ok {
		t.Fatal("located after timeout")
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("Locate blocked for %s", time.Since(start))
	}
}
