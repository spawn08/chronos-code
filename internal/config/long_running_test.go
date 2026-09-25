package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEmbeddedDefaultsRenewLongRunningWork(t *testing.T) {
	cfg, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatal(err)
	}
	lr := cfg.LongRunning
	if !lr.Renewing() || !lr.ToolRoundsPersisted() || lr.NoProgressWindows != 2 || lr.SubagentTimeoutSec != 0 {
		t.Fatalf("long_running defaults = %+v", lr)
	}
	if lr.Window.ToolRounds <= 0 || lr.Window.ToolCalls <= 0 || lr.Window.Seconds <= 0 || lr.Window.Tokens <= 0 {
		t.Fatalf("default window = %+v, want every dimension bounded", lr.Window)
	}
}

func TestLongRunningZeroValueKeepsLegacyBoundedBehavior(t *testing.T) {
	var lr LongRunningConfig
	if lr.Renewing() || lr.ToolRoundsPersisted() {
		t.Fatalf("zero value = %+v, want bounded legacy behavior", lr)
	}
}

func TestLongRunningOverlayMergesOnlyDeclaredFields(t *testing.T) {
	base, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatal(err)
	}
	overlay := mustConfig(t, "long_running:\n  window:\n    seconds: 60\n")
	mergeConfig(base, overlay, "project")
	if base.LongRunning.Window.Seconds != 60 || !base.LongRunning.Renewing() || base.LongRunning.Window.ToolRounds != 60 {
		t.Fatalf("merged long_running = %+v", base.LongRunning)
	}
	bounded := mustConfig(t, "long_running:\n  mode: bounded\n")
	mergeConfig(base, bounded, "project")
	if base.LongRunning.Renewing() {
		t.Fatal("bounded overlay did not disable renewal")
	}
}

func TestLongRunningConfigValidation(t *testing.T) {
	for _, body := range []string{
		"long_running:\n  mode: forever\n",
		"long_running:\n  no_progress_windows: -1\n",
		"long_running:\n  subagent_timeout_sec: -1\n",
		"long_running:\n  window:\n    tool_rounds: -1\n",
		"long_running:\n  window:\n    tokens: -5\n",
	} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(body), &cfg); err == nil || !strings.Contains(err.Error(), "long_running") {
			t.Fatalf("%q error = %v, want long_running validation", body, err)
		}
	}
}
