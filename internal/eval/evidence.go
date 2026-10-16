package eval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

const RunRecordVersion = "chronos.eval.run.v1"

// SourceSnapshot fingerprints source bytes, including staged and unstaged work.
type SourceSnapshot struct {
	Revision   string `json:"revision"`
	Dirty      bool   `json:"dirty"`
	SourceHash string `json:"source_sha256"`
	Status     string `json:"status"`
}

type RunMetadata struct {
	BinaryHash          string               `json:"binary_sha256"`
	BuildSettings       []debug.BuildSetting `json:"build_settings"`
	ProviderFixtureHash string               `json:"provider_fixture_sha256,omitempty"`
	EffectiveConfigHash string               `json:"effective_config_sha256"`
	EffectiveConfig     json.RawMessage      `json:"effective_config_redacted"`
	Code                SourceSnapshot       `json:"code"`
	SDK                 SourceSnapshot       `json:"sdk"`
	SDKModule           string               `json:"sdk_module"`
	GoVersion           string               `json:"go_version"`
	Platform            string               `json:"platform"`
	ConfigHash          string               `json:"config_sha256"`
	Config              json.RawMessage      `json:"config_redacted"`
	ManifestHash        string               `json:"manifest_sha256"`
	FixtureHash         string               `json:"fixture_sha256"`
	PromptHash          string               `json:"task_prompt_sha256"`
	OracleHash          string               `json:"oracle_sha256"`
	Permissions         []string             `json:"permissions"`
	Ablation            Ablation             `json:"ablation"`
	Environment         []string             `json:"environment"`
}

// RunRecord separates objective outcome from the limits of its observations.
type RunRecord struct {
	FixtureScriptComplete *bool       `json:"fixture_script_complete,omitempty"`
	Version               string      `json:"version"`
	Mode                  string      `json:"mode"`
	StartedAt             time.Time   `json:"started_at"`
	EndedAt               time.Time   `json:"ended_at"`
	Metadata              RunMetadata `json:"metadata"`
	Outcome               TaskOutcome `json:"outcome"`
	Usage                 Usage       `json:"usage"`
	ModelRounds           int         `json:"model_rounds"`
	ExecutedToolCalls     int         `json:"executed_tool_calls"`
	UnpricedCallIDs       []string    `json:"unpriced_call_ids"`
	UsageEvidence         string      `json:"usage_evidence"`
	Blocked               []string    `json:"blocked"`
}

func hashBytes(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

// CaptureRunMetadata records the configured input before execution; secrets are excluded.
func CaptureRunMetadata(ctx context.Context, root, manifest string, cfg any, run TaskRun) (RunMetadata, error) {
	code, err := snapshotSource(ctx, root)
	if err != nil {
		return RunMetadata{}, err
	}
	command := exec.CommandContext(ctx, "go", "list", "-m", "-json", "github.com/spawn08/chronos")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return RunMetadata{}, fmt.Errorf("eval: resolve SDK module: %w", err)
	}
	var module struct {
		Path, Version, Dir string
		Replace            *struct{ Path, Version, Dir string }
	}
	if err := json.Unmarshal(output, &module); err != nil {
		return RunMetadata{}, fmt.Errorf("eval: decode SDK module: %w", err)
	}
	sdkDir := module.Dir
	sdkVersion := module.Path + "@" + module.Version
	if module.Replace != nil {
		sdkDir = module.Replace.Dir
		sdkVersion += " => " + module.Replace.Path + "@" + module.Replace.Version
	}
	sdk, err := snapshotSource(ctx, sdkDir)
	if err != nil {
		return RunMetadata{}, fmt.Errorf("eval: snapshot SDK: %w", err)
	}
	configJSON, err := RedactedConfig(cfg)
	if err != nil {
		return RunMetadata{}, err
	}
	manifestData, err := os.ReadFile(manifest)
	if err != nil {
		return RunMetadata{}, fmt.Errorf("eval: read manifest metadata: %w", err)
	}
	oracle, err := json.Marshal(run.Grading)
	if err != nil {
		return RunMetadata{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return RunMetadata{}, err
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return RunMetadata{}, err
	}
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return RunMetadata{BinaryHash: hashBytes(binary), BuildSettings: settings, Code: code, SDK: sdk, SDKModule: sdkVersion, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		ConfigHash: hashBytes(configJSON), Config: configJSON, ManifestHash: hashBytes(manifestData), PromptHash: hashBytes([]byte(run.Prompt)), OracleHash: hashBytes(oracle),
		Permissions: run.Permissions, Ablation: run.Ablation, Environment: run.Environment}, nil
}

func (m *RunMetadata) CaptureProviderFixture(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m.ProviderFixtureHash = hashBytes(data)
	return nil
}

func (m *RunMetadata) CaptureEffectiveConfig(cfg any) error {
	data, err := RedactedConfig(cfg)
	if err != nil {
		return err
	}
	m.EffectiveConfig = data
	m.EffectiveConfigHash = hashBytes(data)
	return nil
}

func RedactedConfig(cfg any) (json.RawMessage, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("eval: encode config: %w", err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	redactValue(value)
	return json.Marshal(value)
}

func redactValue(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if strings.Contains(normalized, "apikey") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") || normalized == "token" || normalized == "authorization" || normalized == "headers" || normalized == "env" || normalized == "dsn" {
				v[key] = "<redacted>"
			} else {
				redactValue(child)
			}
		}
	case []any:
		for _, child := range v {
			redactValue(child)
		}
	}
}

func snapshotSource(ctx context.Context, root string) (SourceSnapshot, error) {
	absoluteRoot, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("eval: locate source root: %w", err)
	}
	root = strings.TrimSpace(string(absoluteRoot))
	git := func(args ...string) ([]byte, error) {
		output, err := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return nil, fmt.Errorf("eval: git metadata %s: %w", strings.Join(args, " "), err)
		}
		return output, nil
	}
	revision, err := git("rev-parse", "HEAD")
	if err != nil {
		return SourceSnapshot{}, err
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return SourceSnapshot{}, err
	}
	files, err := git("ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return SourceSnapshot{}, err
	}
	paths := strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00")
	sort.Strings(paths)
	h := sha256.New()
	seen := map[string]bool{}
	for _, path := range paths {
		ext := filepath.Ext(path)
		if seen[path] || !(ext == ".go" || ext == ".yaml" || ext == ".yml" || ext == ".mod" || ext == ".sum" || path == "Makefile") {
			continue
		}
		seen[path] = true
		data, err := os.ReadFile(filepath.Join(root, path))
		if os.IsNotExist(err) {
			data = []byte("<deleted>")
		} else if err != nil {
			return SourceSnapshot{}, err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(data))
		h.Write(data)
	}
	return SourceSnapshot{Revision: strings.TrimSpace(string(revision)), Dirty: len(status) != 0, SourceHash: fmt.Sprintf("%x", h.Sum(nil)), Status: string(status)}, nil
}

// FixtureHash hashes relative paths and content without timestamps or temp roots.
func FixtureHash(root string) (string, error) {
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if relative == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("eval: non-regular fixture file %s", relative)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", filepath.ToSlash(relative), info.Mode().Perm(), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func NewRunRecord(mode string, metadata RunMetadata, outcome TaskOutcome, started, ended time.Time) RunRecord {
	record := RunRecord{Version: RunRecordVersion, Mode: mode, Metadata: metadata, Outcome: outcome, StartedAt: started, EndedAt: ended, Usage: outcome.Usage(), UnpricedCallIDs: []string{},
		UsageEvidence: "trace-reported; provider billing and per-call prices not verified", Blocked: []string{
			"emitted tool calls, retries, repair rounds, duplicate reads, stalls and artifact-only rounds are not fully observed by this adapter",
			"per-call usage-known and provider pricing metadata are unavailable; zero cost does not establish free execution",
			"paired live-model, unseen holdout, crash/restart and browser benchmarks not executed",
			"durable session terminal events and complete partial-trace preservation on adapter errors require orchestrator/adapter integration",
			"source fingerprints describe the observed working trees, not an immutable build snapshot; binary_sha256 identifies the executed code",
			"inherited environment and host toolchains are not hermetic; environment contains only declared task overrides",
		}}
	seen := map[string]bool{}
	for _, call := range outcome.Calls {
		if seen[call.ID] {
			continue
		}
		seen[call.ID] = true
		switch call.Kind {
		case CallModel:
			if call.ID == "model-terminal" {
				record.Blocked = append(record.Blocked, "terminal usage fallback is not an observed model round; model_rounds excludes it")
			} else {
				record.ModelRounds++
			}
			record.UnpricedCallIDs = append(record.UnpricedCallIDs, call.ID)
		case CallTool:
			record.ExecutedToolCalls++
		}
	}
	if len(outcome.RequestContexts) == 0 {
		record.Blocked = append(record.Blocked, "runtime prompt/schema hashes missing: integrate request metrics before/after every provider call")
	}
	if mode == "fixture" {
		record.UsageEvidence = "scripted provider; synthetic zero usage, no billable model invocation"
		record.UnpricedCallIDs = []string{}
	}
	return record
}
