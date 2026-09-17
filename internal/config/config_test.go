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
	t.Setenv("VIEWER_SALT", "test-salt")
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

// minimalEnv sets just enough for Load to succeed, with images disabled so
// Bunny vars are not required.
func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ZILLOW_API_KEY", "test-key")
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("LOCATIONIQ_API_KEY", "")
	t.Setenv("SEARCH_LOCATION", "")
	t.Setenv("API_BUDGET_PER_CYCLE", "")
	t.Setenv("VIEWER_SALT", "test-salt")
}

func TestLoad_APIBudgetDefault(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 150 {
		t.Errorf("APIBudgetPerCycle = %d, want 150", cfg.APIBudgetPerCycle)
	}
}

func TestLoad_APIBudgetFromEnv(t *testing.T) {
	minimalEnv(t)
	t.Setenv("API_BUDGET_PER_CYCLE", "300")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 300 {
		t.Errorf("APIBudgetPerCycle = %d, want 300", cfg.APIBudgetPerCycle)
	}
}

func TestLoad_SearchLocationNotRequired(t *testing.T) {
	minimalEnv(t)
	if _, err := Load(); err != nil {
		t.Fatalf("Load without SEARCH_LOCATION: %v", err)
	}
}

func TestLoad_LinearDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("ZILLOW_API_KEY", "k")
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("PUBLIC_BASE_URL", "https://api.example.com/")
	t.Setenv("VIEWER_SALT", "s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Linear.Enabled || cfg.Linear.LineupHours != 6 || cfg.Linear.MinScopeClips != 20 || cfg.Linear.EPGHorizonHours != 24 {
		t.Errorf("linear defaults = %+v", cfg.Linear)
	}
	if cfg.PublicBaseURL != "https://api.example.com" {
		t.Errorf("PublicBaseURL = %q (trailing slash must be trimmed)", cfg.PublicBaseURL)
	}
}

func TestLoad_ViewerSaltRequiredWithTracking(t *testing.T) {
	minimalEnv(t)
	t.Setenv("VIEWER_SALT", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VIEWER_SALT") {
		t.Errorf("tracking on without a salt must fail at startup, got %v", err)
	}
	t.Setenv("VIEWER_TRACKING_ENABLED", "false")
	if _, err := Load(); err != nil {
		t.Errorf("tracking off needs no salt: %v", err)
	}
	t.Setenv("VIEWER_TRACKING_ENABLED", "")
	t.Setenv("LINEAR_ENABLED", "false")
	if _, err := Load(); err != nil {
		t.Errorf("no linear channels, no tracking, no salt needed: %v", err)
	}
}

func TestLoad_ViewerDefaults(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Viewer
	if !v.Enabled || v.Salt != "test-salt" || v.RotateDaily || v.RetentionDays != 30 || v.GeoIPDBPath != "" {
		t.Errorf("viewer defaults = %+v", v)
	}
	t.Setenv("VIEWER_SALT_ROTATE_DAILY", "true")
	t.Setenv("VIEWER_RETENTION_DAYS", "0")
	t.Setenv("GEOIP_DB_PATH", " /geoip/x.mmdb ")
	cfg, _ = Load()
	if !cfg.Viewer.RotateDaily || cfg.Viewer.RetentionDays != 1 || cfg.Viewer.GeoIPDBPath != "/geoip/x.mmdb" {
		t.Errorf("viewer env = %+v", cfg.Viewer)
	}
}

func TestLoad_AdTags(t *testing.T) {
	minimalEnv(t)
	const pre = "https://ads.example.com/vast?pod=pre&cb=[CACHEBUSTER]&did=ROKU_ADS_TRACKING_ID"
	t.Setenv("AD_PREROLL_URL", " "+pre+" ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ads.PrerollURL != pre || cfg.Ads.MidrollURL != "" {
		t.Errorf("ads = %+v", cfg.Ads)
	}
}

func TestLoad_AdTagMustBeAbsoluteHTTPURL(t *testing.T) {
	for _, bad := range []string{"ads.example.com/vast", "ftp://ads.example.com/vast", "https://"} {
		t.Run(bad, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("AD_MIDROLL_URL", bad)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "AD_MIDROLL_URL") {
				t.Errorf("Load() error = %v, want one naming AD_MIDROLL_URL", err)
			}
		})
	}
}
