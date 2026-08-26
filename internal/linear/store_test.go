package linear

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory Store for tests.
type memStore struct {
	mu       sync.Mutex
	clips    map[int64]memClip
	versions map[string][]Version
	zipCity  map[string][2]string
	zips     map[string]bool // ZIPs that exist, with or without listings
	cities   map[string]bool // "city|state" pairs that exist
}

type memClip struct {
	seg   ClipSegments
	scope Scope // where the listing is: Zip, City, State all set
	price int64
}

func newMemStore() *memStore {
	return &memStore{
		clips: map[int64]memClip{}, versions: map[string][]Version{},
		zipCity: map[string][2]string{}, zips: map[string]bool{}, cities: map[string]bool{},
	}
}

// addZip registers a ZIP that exists but has no listings of its own.
func (m *memStore) addZip(zip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.zips[zip] = true
}

// addClip registers clip id in scope sc (Zip, City and State should all be
// set) with the given segment durations.
func (m *memStore) addClip(id int64, sc Scope, price int64, segMS ...int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clips[id] = memClip{
		seg:   ClipSegments{ID: id, BaseURL: fmt.Sprintf("https://cdn/hls/v1/%d/x", id), SegmentMS: segMS},
		scope: sc, price: price,
	}
	if sc.Zip != "" {
		m.zipCity[sc.Zip] = [2]string{sc.City, sc.State}
		m.zips[sc.Zip] = true
	}
	if sc.City != "" {
		m.cities[sc.City+"|"+sc.State] = true
	}
}

// AreaExists mirrors the repository: a ZIP exists when it is in zip_codes, a
// city+state when some property is in it; national and (already validated)
// state scopes always exist.
func (m *memStore) AreaExists(_ context.Context, sc Scope) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case sc.Zip != "":
		return m.zips[sc.Zip], nil
	case sc.City != "":
		return m.cities[sc.City+"|"+sc.State], nil
	}
	return true, nil
}

func matches(want, have Scope) bool {
	switch {
	case want.Zip != "":
		return have.Zip == want.Zip
	case want.City != "":
		return have.City == want.City && have.State == want.State
	case want.State != "":
		return have.State == want.State
	}
	return true
}

func (m *memStore) ListClips(_ context.Context, s Scope) ([]ClipRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ClipRef
	for id, c := range m.clips {
		if !matches(s, c.scope) {
			continue
		}
		total := 0
		for _, ms := range c.seg.SegmentMS {
			total += ms
		}
		out = append(out, ClipRef{ID: id, TotalMS: total, Segments: len(c.seg.SegmentMS)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) CityOfZip(_ context.Context, zip string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.zipCity[zip]
	return cs[0], cs[1], nil
}

func (m *memStore) LatestVersions(_ context.Context, key string, n int) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := append([]Version(nil), m.versions[key]...)
	sort.Slice(vs, func(i, j int) bool { return vs[i].Version > vs[j].Version })
	if len(vs) > n {
		vs = vs[:n]
	}
	return vs, nil
}

func (m *memStore) VersionsAt(_ context.Context, key string, t time.Time) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var vs []Version
	for _, v := range m.versions[key] {
		if !v.StartsAt.After(t) {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Version > vs[j].Version })
	if len(vs) > 2 {
		vs = vs[:2]
	}
	return vs, nil
}

func (m *memStore) ListVersionsBetween(_ context.Context, key string, from, to time.Time) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var vs []Version
	for _, v := range m.versions[key] {
		if !v.EndsAt.Before(from) && v.StartsAt.Before(to) {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Version < vs[j].Version })
	return vs, nil
}

// ListVersions is not part of Store; the tests use it to inspect how much of
// a chain a call materialised.
func (m *memStore) ListVersions(_ context.Context, key string) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := append([]Version(nil), m.versions[key]...)
	sort.Slice(vs, func(i, j int) bool { return vs[i].Version < vs[j].Version })
	return vs, nil
}

func (m *memStore) InsertVersion(_ context.Context, v *Version) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.versions[v.Key] {
		if e.Version == v.Version {
			return false, nil
		}
	}
	m.versions[v.Key] = append(m.versions[v.Key], *v)
	return true, nil
}

func (m *memStore) ClipsByID(_ context.Context, ids []int64) (map[int64]ClipSegments, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int64]ClipSegments{}
	for _, id := range ids {
		if c, ok := m.clips[id]; ok {
			out[id] = c.seg
		}
	}
	return out, nil
}

func (m *memStore) ListingsByClipID(_ context.Context, ids []int64) ([]Listing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Listing
	for _, id := range ids {
		if c, ok := m.clips[id]; ok {
			out = append(out, Listing{ClipID: id, ZPID: fmt.Sprint(id), Price: c.price, City: c.scope.City, State: c.scope.State})
		}
	}
	return out, nil
}
