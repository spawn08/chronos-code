//go:build !lsp

package lsp

import (
	"strings"
	"testing"
)

func TestNewClient_StubReturnsError(t *testing.T) {
	client, err := NewClient("gopls")
	if err == nil {
		t.Fatal("expected error from stub NewClient")
	}
	if !strings.Contains(err.Error(), "not compiled") {
		t.Errorf("error should mention 'not compiled', got: %v", err)
	}
	if client != nil {
		t.Error("expected nil client from stub")
	}
}

func TestClose_StubNoError(t *testing.T) {
	var c Client
	if err := c.Close(); err != nil {
		t.Errorf("stub Close should not error, got: %v", err)
	}
}

func TestTools_StubReturnsNil(t *testing.T) {
	tools := Tools(nil, "/tmp")
	if tools != nil {
		t.Errorf("stub Tools should return nil, got %d tools", len(tools))
	}
}
