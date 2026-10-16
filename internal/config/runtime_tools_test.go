package config

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestRuntimeToolsEmbeddedDefaults(t *testing.T) {
	cfg, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tools.Journal || !cfg.Tools.DeferredSchemas || !cfg.Tools.LeanContext || cfg.Tools.YieldMilliseconds <= 0 || cfg.Tools.MaxBackgroundTools <= 0 || cfg.Tools.UnaryOutputTokens <= 0 {
		t.Fatalf("recovery defaults are incomplete: %+v", cfg.Tools)
	}
	core := map[string]bool{}
	for _, name := range cfg.Tools.CoreTools {
		core[name] = true
	}
	for _, name := range []string{"file_read", "file_write", "shell", "read_artifact", "read_history", "await_tool", "cancel_tool", "discover_tools", "call_tool"} {
		if !core[name] {
			t.Errorf("missing core tool %s", name)
		}
	}
}

func TestRuntimeToolsOverlayDisablesFlagsExplicitly(t *testing.T) {
	base, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatal(err)
	}
	mergeConfig(base, mustConfig(t, "tools:\n  journal: false\n  deferred_schemas: false\n  lean_context: false\n  yield_ms: 0\n"), "project")
	if base.Tools.Journal || base.Tools.DeferredSchemas || base.Tools.LeanContext || base.Tools.YieldMilliseconds != 0 {
		t.Fatalf("explicit recovery opt-outs lost: %+v", base.Tools)
	}
	if base.Tools.MaxBackgroundTools <= 0 {
		t.Fatal("overlay lost undeclared capacity")
	}
}

func TestRuntimeToolsRejectInvalidResourceBounds(t *testing.T) {
	for _, text := range []string{"tools:\n  yield_ms: -1\n", "tools:\n  max_background_tools: -1\n", "tools:\n  max_background_tools: 65\n"} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(text), &cfg); err == nil {
			t.Fatalf("accepted invalid bounds %q", text)
		}
	}
}
