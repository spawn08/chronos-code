package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestStripGlobalFlagsYoloPositions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "before subcommand",
			args: []string{"chronos-code", "--yolo", "run", "task"},
			want: []string{"chronos-code", "run", "task"},
		},
		{
			name: "after subcommand",
			args: []string{"chronos-code", "run", "--yolo", "task"},
			want: []string{"chronos-code", "run", "task"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobalFlags(t, tt.args)

			if err := stripGlobalFlags(); err != nil {
				t.Fatalf("stripGlobalFlags() error = %v", err)
			}
			if !yoloMode {
				t.Fatal("yoloMode = false, want true")
			}
			if got := effectivePermissionMode(); got != "auto_approve" {
				t.Fatalf("effectivePermissionMode() = %q, want auto_approve", got)
			}
			if got := strings.Join(os.Args, "\x00"); got != strings.Join(tt.want, "\x00") {
				t.Fatalf("os.Args = %q, want %q", os.Args, tt.want)
			}
		})
	}
}

func TestStripGlobalFlagsRejectsYoloDenyConflict(t *testing.T) {
	tests := [][]string{
		{"chronos-code", "--yolo", "run", "--permission-mode", "deny", "task"},
		{"chronos-code", "run", "--permission-mode=deny", "--yolo", "task"},
	}

	for _, args := range tests {
		resetGlobalFlags(t, args)

		err := stripGlobalFlags()
		if err == nil || !strings.Contains(err.Error(), "--yolo conflicts with --permission-mode deny") {
			t.Fatalf("stripGlobalFlags() error = %v, want yolo/deny conflict", err)
		}
	}
}

func TestStripGlobalFlagsRetainsPermissionModes(t *testing.T) {
	for _, mode := range []string{"prompt", "auto_approve", "deny"} {
		t.Run(mode, func(t *testing.T) {
			resetGlobalFlags(t, []string{"chronos-code", "run", "--permission-mode=" + mode, "task"})

			if err := stripGlobalFlags(); err != nil {
				t.Fatalf("stripGlobalFlags() error = %v", err)
			}
			if permissionMode != mode {
				t.Fatalf("permissionMode = %q, want %q", permissionMode, mode)
			}
		})
	}
}

func TestPrintUsageDocumentsYoloSafetyBoundary(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code"})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
		r.Close()
	})

	if err := printUsage(); err != nil {
		t.Fatalf("printUsage() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close usage writer: %v", err)
	}
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	usage := string(output)
	for _, want := range []string{"--yolo", "policy-allowed", "never overrides deny or destructive confirm"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

func resetGlobalFlags(t *testing.T, args []string) {
	t.Helper()
	originalArgs := os.Args
	originalConfigPath := configPath
	originalDebugMode := debugMode
	originalStreamMode := streamMode
	originalPermissionMode := permissionMode
	originalYoloMode := yoloMode
	originalResumeSessionID := resumeSessionID

	os.Args = append([]string(nil), args...)
	configPath = ""
	debugMode = false
	streamMode = true
	permissionMode = ""
	yoloMode = false
	resumeSessionID = ""

	t.Cleanup(func() {
		os.Args = originalArgs
		configPath = originalConfigPath
		debugMode = originalDebugMode
		streamMode = originalStreamMode
		permissionMode = originalPermissionMode
		yoloMode = originalYoloMode
		resumeSessionID = originalResumeSessionID
	})
}
