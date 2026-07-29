package config

import (
	"strings"
	"testing"
)

// setRequiredEnv sets the environment variables Load requires regardless of
// what this test is exercising, so each test only has to set what's relevant
// to it.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("ZILLOW_API_KEY", "zillow-key")
	t.Setenv("SEARCH_LOCATION", "33950")
}

// TestLoad_LocationIQWithoutImagesRequiresBunny covers finding 2: enabling
// maps (LOCATIONIQ_API_KEY set) with IMAGES_ENABLED=false and no Bunny
// config must fail loudly at startup, not silently burn LocationIQ quota on
// every admitted generation that then fails at the upload step.
func TestLoad_LocationIQWithoutImagesRequiresBunny(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("LOCATIONIQ_API_KEY", "pk.test")
	t.Setenv("BUNNY_STORAGE_ZONE", "")
	t.Setenv("BUNNY_API_KEY", "")
	t.Setenv("BUNNY_CDN_BASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("want error when LOCATIONIQ_API_KEY is set without Bunny config, got nil")
	}
	for _, want := range []string{"BUNNY_STORAGE_ZONE", "BUNNY_API_KEY", "BUNNY_CDN_BASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name missing variable %q", err, want)
		}
	}
}

// TestLoad_LocationIQWithoutImagesSucceedsWithBunny is the positive
// counterpart: the same LOCATIONIQ_API_KEY + IMAGES_ENABLED=false setup
// succeeds once Bunny is configured.
func TestLoad_LocationIQWithoutImagesSucceedsWithBunny(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("LOCATIONIQ_API_KEY", "pk.test")
	t.Setenv("BUNNY_STORAGE_ZONE", "zone")
	t.Setenv("BUNNY_API_KEY", "key")
	t.Setenv("BUNNY_CDN_BASE_URL", "https://cdn.example.com")

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
