package config

import "testing"

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
