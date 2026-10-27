package viewer

import "testing"

func TestKnownPlatform(t *testing.T) {
	for name, want := range map[string]bool{
		"cineplex": true,
		"Cineplex": false, // one URL per platform
		"":         false,
		"pluto":    false,
	} {
		if got := KnownPlatform(name); got != want {
			t.Errorf("KnownPlatform(%q) = %v, want %v", name, got, want)
		}
	}
}
