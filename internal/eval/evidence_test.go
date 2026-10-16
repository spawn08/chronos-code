package eval

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"gopkg.in/yaml.v3"
)

func TestRunMetadataRedactsSecretsAndKeepsConfiguration(t *testing.T) {
	data, err := RedactedConfig(map[string]any{"model": "fixture-v1", "api_key": "secret-value", "nested": []any{map[string]any{"APIKey": "nested-secret", "Headers": map[string]string{"Authorization": "bearer-secret"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-value", "nested-secret", "bearer-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("leaked secret: %s", data)
		}
	}
	if !strings.Contains(string(data), "fixture-v1") {
		t.Fatalf("lost model: %s", data)
	}
}

func TestSourceSnapshotPreservesAndFingerprintsDirtyWork(t *testing.T) {
	root, _ := taskRunnerFixture(t)
	source := filepath.Join(root, "source.go")
	if err := os.WriteFile(source, []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := snapshotSource(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Dirty || len(first.SourceHash) != 64 {
		t.Fatalf("snapshot: %+v", first)
	}
	if err := os.WriteFile(source, []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := snapshotSource(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceHash == second.SourceHash || first.Revision != second.Revision {
		t.Fatalf("snapshots: %+v %+v", first, second)
	}
	if output, err := exec.Command("git", "-C", root, "diff", "--cached", "--name-only").Output(); err != nil || len(output) != 0 {
		t.Fatalf("metadata changed index: %s %v", output, err)
	}
}

func TestFixtureHashIndependentOfRootAndSensitiveToContents(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first, err := FixtureHash(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FixtureHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("fixture hash depends on temporary root")
	}
	if err := os.WriteFile(filepath.Join(b, "answer.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := FixtureHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if first == changed {
		t.Fatal("changed fixture did not change hash")
	}
}

func TestRunRecordPreservesUsageClassesAndMarksUnpricedCalls(t *testing.T) {
	outcome := TaskOutcome{Calls: []Call{{ID: "m1", Kind: CallModel, Usage: Usage{InputTokens: 3, CacheTokens: 12, CacheReadTokens: 7, CacheWriteTokens: 5, OutputTokens: 2}}, {ID: "t1", Kind: CallTool}}}
	record := NewRunRecord("live", RunMetadata{}, outcome, time.Now(), time.Now())
	if record.Usage.CacheReadTokens != 7 || record.Usage.CacheWriteTokens != 5 || record.Usage.CacheTokens != 12 || record.ModelRounds != 1 || record.ExecutedToolCalls != 1 || len(record.UnpricedCallIDs) != 1 || len(record.Blocked) == 0 {
		t.Fatalf("record: %+v", record)
	}
	fixture := NewRunRecord("fixture", RunMetadata{}, outcome, time.Now(), time.Now())
	if len(fixture.UnpricedCallIDs) != 0 || !strings.Contains(fixture.UsageEvidence, "synthetic") {
		t.Fatalf("fixture pricing evidence: %+v", fixture)
	}
}

func TestConfiguredFixtureThroughActualOrchestrator(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	root := filepath.Join("..", "..", "benchmark", "runtime")
	manifest, err := LoadTaskManifest(filepath.Join(root, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var patches []string
	for range 2 {
		provider, err := NewFixtureProvider(filepath.Join(root, "provider.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg config.Config
		if err := yaml.Unmarshal(configData, &cfg); err != nil {
			provider.Close()
			t.Fatal(err)
		}
		if err := provider.Configure(&cfg); err != nil {
			provider.Close()
			t.Fatal(err)
		}
		run, err := manifest.Tasks[0].TaskRun(provider.Model(), "fixture-test")
		if err != nil {
			provider.Close()
			t.Fatal(err)
		}
		runner := TaskRunner{Adapter: ChronosCodeAdapter{Execute: LiveExecutor(func() (*config.Config, error) { return &cfg, nil })}}
		result, err := runner.Run(context.Background(), run)
		complete := provider.Complete()
		provider.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !result.Outcome.Grader.Passed || !complete {
			encoded, _ := json.Marshal(result.Outcome)
			t.Fatalf("fixture failed: %s, complete=%v provider=%s", encoded, complete, provider.Failure())
		}
		if err := result.Outcome.Validate(); err != nil {
			t.Fatal(err)
		}
		if len(result.Outcome.RequestContexts) != 4 {
			t.Fatalf("runtime prompt/schema observations = %d, want 4", len(result.Outcome.RequestContexts))
		}
		record := NewRunRecord("fixture", RunMetadata{}, result.Outcome, time.Now(), time.Now())
		if record.ModelRounds != 4 || record.ExecutedToolCalls != 3 {
			t.Fatalf("actual trace counts: %+v", record)
		}
		patches = append(patches, result.Patch)
	}
	if patches[0] != patches[1] || !strings.Contains(patches[0], "+resolved") {
		t.Fatalf("non-repeatable patches: %q %q", patches[0], patches[1])
	}
	original, err := os.ReadFile(filepath.Join(root, "fixture", "answer.txt"))
	if err != nil || string(original) != "original\n" {
		t.Fatalf("source fixture mutated: %q %v", original, err)
	}
}
