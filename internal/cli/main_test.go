package cli

import (
	"os"
	"testing"

	"github.com/spawn08/chronos-code/internal/modelsdev"
)

func TestMain(m *testing.M) {
	// Commands that build an orchestrator start a background models.dev
	// refresh; tests must never reach the network.
	os.Setenv(modelsdev.DisableFetchEnv, "1")
	os.Exit(m.Run())
}
