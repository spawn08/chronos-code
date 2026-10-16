package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfiguredFixtureOracleRejectsConfidentButIncorrectFinal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	root := filepath.Join("..", "..", "benchmark", "runtime")
	manifest, err := LoadTaskManifest(filepath.Join(root, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "provider.yaml")
	if err := os.WriteFile(script, []byte("version: chronos.eval.provider.v1\nmodel: fixture-script-v1\nresponses:\n  - content: Changed answer.txt and verified it successfully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider, err := NewFixtureProvider(script)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := provider.Configure(&cfg); err != nil {
		t.Fatal(err)
	}
	run, err := manifest.Tasks[0].TaskRun(provider.Model(), "incorrect-final")
	if err != nil {
		t.Fatal(err)
	}
	runner := TaskRunner{Adapter: ChronosCodeAdapter{Execute: LiveExecutor(func() (*config.Config, error) { return &cfg, nil })}}
	result, err := runner.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Grader.Passed || result.Outcome.Failure == nil || result.Outcome.Failure.Class != FailureGrader || result.Patch != "" {
		t.Fatalf("incorrect final passed objective grading: %+v", result)
	}
	if len(result.Outcome.Verification) == 0 || result.Outcome.Verification[0].Passed {
		t.Fatalf("missing failed oracle: %+v", result.Outcome.Verification)
	}
}

func TestConfiguredFixtureProviderRejectsInvalidScripts(t *testing.T) {
	for _, data := range []string{
		"version: unknown\nmodel: fixture\nresponses: []\n",
		"version: chronos.eval.provider.v1\nmodel: fixture\nresponses: []\n",
		"version: chronos.eval.provider.v1\nmodel: fixture\nresponses: [{content: done}]\nunknown: true\n",
	} {
		path := filepath.Join(t.TempDir(), "provider.yaml")
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		provider, err := NewFixtureProvider(path)
		if err == nil {
			provider.Close()
			t.Fatalf("accepted invalid script: %s", data)
		}
	}
}
