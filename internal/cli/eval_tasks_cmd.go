package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"gopkg.in/yaml.v3"
	"strings"

	"github.com/spawn08/chronos-code/internal/eval"
)

const defaultTaskManifestPath = "benchmark/tasks/manifest-v1.yaml"

func runEvalTasks(args []string) error {
	return runEvalTasksTo(args, os.Stdout)
}

func runEvalTasksTo(args []string, stdout io.Writer) error {
	manifestPath := defaultTaskManifestPath
	validateOnly := false
	runLive := false
	repetitions := 1
	fixtureProvider := ""
	outputPath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--run":
			runLive = true
		case "--fixture-provider", "--output":
			flag := args[i]
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a path", flag)
			}
			i++
			if flag == "--fixture-provider" {
				fixtureProvider = args[i]
			} else {
				outputPath = args[i]
			}
		case "--repetitions":
			if i+1 >= len(args) {
				return fmt.Errorf("--repetitions requires a number")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > 100 {
				return fmt.Errorf("--repetitions must be 1–100")
			}
			repetitions = n
		case "--validate-only":
			validateOnly = true
		case "--manifest":
			if i+1 >= len(args) {
				return fmt.Errorf("--manifest requires a path")
			}
			i++
			manifestPath = args[i]
		default:
			if strings.HasPrefix(args[i], "--manifest=") && len(args[i]) > len("--manifest=") {
				manifestPath = strings.TrimPrefix(args[i], "--manifest=")
				continue
			}
			return fmt.Errorf("unknown eval tasks argument %q", args[i])
		}
	}
	if !validateOnly && !runLive || validateOnly && runLive {
		return fmt.Errorf("usage: chronos-code eval tasks --validate-only | --run [--manifest <path>] [--repetitions <n>]")
	}
	if validateOnly && (fixtureProvider != "" || outputPath != "") {
		return fmt.Errorf("--fixture-provider and --output require --run")
	}
	manifest, err := eval.LoadTaskManifest(manifestPath)
	if err != nil {
		return err
	}
	if runLive {
		factory := loadConfigWithModelSelection
		mode := "live"
		if fixtureProvider != "" {
			if configPath == "" {
				return fmt.Errorf("fixture evaluation requires an explicit --config")
			}
			mode = "fixture"
			factory = func() (*config.Config, error) {
				data, err := os.ReadFile(configPath)
				if err != nil {
					return nil, err
				}
				var cfg config.Config
				if err := yaml.Unmarshal(data, &cfg); err != nil {
					return nil, err
				}
				return &cfg, nil
			}
		}
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		if outputPath != "" {
			if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create evaluation output (must not exist): %w", err)
			}
			defer file.Close()
			stdout = file
		}
		encoder := json.NewEncoder(stdout)
		failed := false
		for _, task := range manifest.Tasks {
			for repetition := 1; repetition <= repetitions; repetition++ {
				cfg, err := factory()
				if err != nil {
					return err
				}
				configuredConfig, err := eval.RedactedConfig(cfg)
				if err != nil {
					return err
				}
				var provider *eval.FixtureProvider
				if fixtureProvider != "" {
					provider, err = eval.NewFixtureProvider(fixtureProvider)
					if err != nil {
						return err
					}
					if err := provider.Configure(cfg); err != nil {
						provider.Close()
						return err
					}
				}
				_, selected, _, _ := cfg.PrimaryAgentModel()
				settings := eval.ModelSettings{Provider: selected.Provider, Name: selected.Model}
				run, err := task.TaskRun(settings, fmt.Sprintf("%s-%d", task.ID, repetition))
				if err != nil {
					if provider != nil {
						provider.Close()
					}
					return err
				}
				metadata, err := eval.CaptureRunMetadata(context.Background(), root, manifestPath, configuredConfig, run)
				if err != nil {
					if provider != nil {
						provider.Close()
					}
					return err
				}
				if provider != nil {
					if err := metadata.CaptureProviderFixture(fixtureProvider); err != nil {
						provider.Close()
						return err
					}
				}
				// Capture the materialized snapshot rather than a mutable git source.
				executor := eval.LiveExecutor(func() (*config.Config, error) { return cfg, nil })
				runner := eval.TaskRunner{Adapter: eval.ChronosCodeAdapter{Execute: func(ctx context.Context, execution eval.TaskExecution) (eval.EnvelopeRecord, error) {
					metadata.FixtureHash, err = eval.FixtureHash(execution.Workspace)
					if err != nil {
						return eval.EnvelopeRecord{}, err
					}
					record, executeErr := executor(ctx, execution)
					metadataErr := metadata.CaptureEffectiveConfig(cfg)
					if executeErr != nil {
						return record, executeErr
					}
					return record, metadataErr
				}}}
				started := time.Now().UTC()
				result, err := runner.Run(context.Background(), run)
				ended := time.Now().UTC()
				var scriptComplete *bool
				providerFailure := ""
				if provider != nil {
					complete := provider.Complete()
					scriptComplete = &complete
					failed = failed || !complete
					providerFailure = provider.Failure()
					provider.Close()
				}
				if err != nil {
					return err
				}
				record := eval.NewRunRecord(mode, metadata, result.Outcome, started, ended)
				record.FixtureScriptComplete = scriptComplete
				if providerFailure != "" {
					record.Blocked = append(record.Blocked, "fixture provider: "+providerFailure)
				}
				if err := encoder.Encode(record); err != nil {
					return err
				}
				if task.Expected == "failed" {
					failed = failed || result.Outcome.Grader.Passed
				} else {
					failed = failed || !result.Outcome.Grader.Passed
				}
			}
		}
		if failed {
			return fmt.Errorf("evaluation did not satisfy expected objective outcomes; inspect recorded evidence")
		}
		return nil
	}
	_, err = fmt.Fprintf(stdout, "validated %d tasks from %s (%s)\n", len(manifest.Tasks), manifestPath, manifest.Version)
	return err
}
