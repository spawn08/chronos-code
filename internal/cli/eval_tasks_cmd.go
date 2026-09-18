package cli

import (
	"fmt"
	"io"
	"os"
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
	for i := 0; i < len(args); i++ {
		switch args[i] {
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
	if !validateOnly {
		return fmt.Errorf("usage: chronos-code eval tasks --validate-only [--manifest <path>]")
	}
	manifest, err := eval.LoadTaskManifest(manifestPath)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "validated %d tasks from %s (%s)\n", len(manifest.Tasks), manifestPath, manifest.Version)
	return err
}
