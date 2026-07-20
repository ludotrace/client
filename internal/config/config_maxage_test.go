package config

import "testing"

func TestLoad_QueueMaxAgeDays_Default(t *testing.T) {
	withConfigFile(t, `core_url = "https://core.ludotrace.com"`)
	// Ensure no ambient override leaks in from the environment.
	t.Setenv("QUEUE_MAX_AGE_DAYS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.QueueMaxAgeDays != defaultQueueMaxAgeDays {
		t.Errorf("QueueMaxAgeDays = %d, want default %d", cfg.QueueMaxAgeDays, defaultQueueMaxAgeDays)
	}
}

func TestLoad_QueueMaxAgeDays_EnvOverride(t *testing.T) {
	withConfigFile(t, `core_url = "https://core.ludotrace.com"`)
	t.Setenv("QUEUE_MAX_AGE_DAYS", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.QueueMaxAgeDays != 7 {
		t.Errorf("QueueMaxAgeDays = %d, want 7", cfg.QueueMaxAgeDays)
	}
}

// A missing config file must still yield the env-driven max age (the
// early-return default path).
func TestLoad_QueueMaxAgeDays_MissingFile(t *testing.T) {
	orig := loadFrom
	loadFrom = func() (string, error) { return "/no/such/config.toml", nil }
	t.Cleanup(func() { loadFrom = orig })
	t.Setenv("QUEUE_MAX_AGE_DAYS", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.QueueMaxAgeDays != 5 {
		t.Errorf("QueueMaxAgeDays = %d, want 5 (missing-file path)", cfg.QueueMaxAgeDays)
	}
}

func TestLoad_QueueMaxAgeDays_InvalidFallsBack(t *testing.T) {
	for _, v := range []string{"garbage", "0", "-3"} {
		t.Run(v, func(t *testing.T) {
			withConfigFile(t, `core_url = "https://core.ludotrace.com"`)
			t.Setenv("QUEUE_MAX_AGE_DAYS", v)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.QueueMaxAgeDays != defaultQueueMaxAgeDays {
				t.Errorf("QueueMaxAgeDays = %d for %q, want default %d",
					cfg.QueueMaxAgeDays, v, defaultQueueMaxAgeDays)
			}
		})
	}
}
