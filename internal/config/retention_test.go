package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRetentionConfigZeroSemanticsAndValidation(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("retention:\n  enabled: true\n  batch_size: 25\n  policies:\n    events: {max_age_days: 0, max_count: 10, max_bytes: 0}\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Retention.Enabled || cfg.Retention.BatchSize != 25 || cfg.Retention.Policies["events"].MaxCount != 10 {
		t.Fatalf("retention config = %+v", cfg.Retention)
	}
	if err := yaml.Unmarshal([]byte("retention:\n  policies:\n    events: {max_age_days: -1}\n"), &cfg); err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("negative policy error = %v", err)
	}
}
