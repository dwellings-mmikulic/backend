package geo

import (
	"fmt"
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
	loc, ok, _ := c.Locate(net.ParseIP("8.8.8.8"))
	if !ok || loc != (Location{Zip: "94035", City: "Mountain View", State: "ca"}) {
		t.Fatalf("got %+v %v", loc, ok)
	}
}

func TestFreeIPAPI_NonUSIsAMiss(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) {
		return 200, `{"countryCode":"AU","zipCode":"4000","cityName":"Sydney","regionCode":"NSW"}`
	})
	if loc, ok, _ := NewFreeIPAPI(srv.URL+"/api/v1", nil).Locate(net.ParseIP("1.1.1.1")); ok || loc != (Location{}) {
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
		if _, ok, _ := c.Locate(ip); ok {
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
	loc, ok, _ := NewFreeIPAPI(srv.URL+"/api/v1", nil).Locate(net.ParseIP("8.8.4.4"))
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
		if _, ok, _ := c.Locate(net.ParseIP("8.8.8.8")); ok {
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
	if _, ok, _ := c.Locate(net.ParseIP("8.8.8.8")); ok {
		t.Fatal("located after timeout")
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("Locate blocked for %s", time.Since(start))
	}
}

func TestFreeIPAPI_BudgetLimitsUpstreamRequests(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { return 200, usBody })
	client := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	client.SetMaxPerMinute(12) // and so 2 in any 10 s
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }
	ip := func(index int) net.IP { return net.ParseIP(fmt.Sprintf("8.8.%d.%d", index/250, index%250)) }

	// Five new addresses at once: two are asked, three are not, and those
	// three are told so.
	answered := 0
	for index := range 5 {
		if _, ok, got := client.Locate(ip(index)); got {
			answered++
			if !ok {
				t.Errorf("address %d: answered without a location", index)
			}
		}
	}
	if answered != 2 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("answered %d, upstream calls %d, want 2 and 2", answered, calls)
	}
	// An address that was not asked is not remembered as a miss: once there
	// is budget again it is looked up.
	now = now.Add(11 * time.Second)
	if _, ok, got := client.Locate(ip(4)); !ok || !got {
		t.Fatalf("address 4 after the budget recovered: ok %v, answered %v", ok, got)
	}
	for range 100 {
		if _, ok, got := client.Locate(ip(0)); !ok || !got {
			t.Fatal("a cached address stopped answering")
		}
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("upstream calls %d, want 3", calls)
	}
	start := now
	for index := 100; now.Sub(start) < 59*time.Second; index++ {
		client.Locate(ip(index))
		now = now.Add(500 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got > 3+12 {
		t.Fatalf("upstream calls %d in a minute, limit 12 (plus the 3 before)", got)
	}
}

func TestFreeIPAPI_NoBudgetWhenSetToZero(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { return 200, usBody })
	client := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	client.SetMaxPerMinute(0)
	for index := range 80 {
		if _, _, answered := client.Locate(net.ParseIP(fmt.Sprintf("8.8.4.%d", index))); !answered {
			t.Fatalf("lookup %d not answered without a budget", index)
		}
	}
	if atomic.LoadInt32(&calls) != 80 {
		t.Errorf("upstream calls %d, want 80", calls)
	}
}

func TestFreeIPAPI_DefaultBudgetIsUnderTheKeylessAllowance(t *testing.T) {
	var calls int32
	srv := fakeAPI(t, &calls, func(string) (int, string) { return 200, usBody })
	client := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }
	for index := range 600 {
		client.Locate(net.ParseIP(fmt.Sprintf("8.9.%d.%d", index/250, index%250)))
		now = now.Add(100 * time.Millisecond) // 600 addresses over one minute
	}
	if got := atomic.LoadInt32(&calls); got > 60 || got < 30 {
		t.Errorf("upstream calls %d in a minute, want at most the allowance of 60 and most of the budget used", got)
	}
}

func TestFreeIPAPI_LocateTellsAMissFromNoAnswer(t *testing.T) {
	var calls int32
	status := 200
	body := `{"countryCode":"AU","zipCode":"4000","cityName":"Sydney","regionCode":"NSW"}`
	srv := fakeAPI(t, &calls, func(string) (int, string) { return status, body })
	client := NewFreeIPAPI(srv.URL+"/api/v1", nil)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }

	if _, ok, answered := client.Locate(net.ParseIP("1.1.1.1")); ok || !answered {
		t.Errorf("abroad: ok %v, answered %v; want a miss that is an answer", ok, answered)
	}
	if _, ok, answered := client.Locate(net.ParseIP("10.0.0.5")); ok || !answered {
		t.Errorf("private address: ok %v, answered %v; want a miss that is an answer", ok, answered)
	}
	status = 429
	if _, ok, answered := client.Locate(net.ParseIP("8.8.8.8")); ok || answered {
		t.Errorf("refused by the API: ok %v, answered %v; want no answer", ok, answered)
	}
	// The failure is remembered for a minute, still as no answer, then the
	// address is asked again.
	before := atomic.LoadInt32(&calls)
	if _, _, answered := client.Locate(net.ParseIP("8.8.8.8")); answered || atomic.LoadInt32(&calls) != before {
		t.Error("a remembered failure must stay unanswered and cost no request")
	}
	status, body = 200, usBody
	now = now.Add(failureTTL + time.Second)
	if loc, ok, answered := client.Locate(net.ParseIP("8.8.8.8")); !ok || !answered || loc.Zip != "94035" {
		t.Errorf("after the failure expired: %+v ok %v answered %v", loc, ok, answered)
	}
}
