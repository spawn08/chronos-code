package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeliverySandboxProfileRejectsUnknownOrUnpinnedAuthority(t *testing.T) {
	pinned := "alpine@sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name, fields string
		wantErr      bool
	}{
		{"pinned", "image: " + pinned, false},
		{"mutable image", "image: alpine:latest", true},
		{"unknown field", "image: " + pinned + "\n    privileged: true", true},
		{"remote socket", "image: " + pinned + "\n    socket_path: tcp://docker.example:2375", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cfg Config
			err := yaml.Unmarshal([]byte("server:\n  delivery_sandbox:\n    "+test.fields+"\n"), &cfg)
			if (err != nil) != test.wantErr {
				t.Fatalf("unmarshal error = %v, want error %v", err, test.wantErr)
			}
			if err == nil && cfg.Server.DeliverySandbox.Image != pinned {
				t.Fatalf("image = %q", cfg.Server.DeliverySandbox.Image)
			}
		})
	}
}
