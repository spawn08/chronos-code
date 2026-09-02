package tui

import "testing"

func TestApprovalCacheIsSessionScoped(t *testing.T) {
	cache := newApprovalCache()
	cache.remember("session-1", "file_read", approvalDecision{allow: true, always: true})

	if !cache.allowed("session-1", "file_read") {
		t.Fatal("remembered tool is not allowed in its session")
	}
	if cache.allowed("session-1", "shell") {
		t.Fatal("tool-specific approval allowed another tool")
	}
	if cache.allowed("session-2", "file_read") {
		t.Fatal("tool approval leaked into another session")
	}

	cache.remember("session-1", "shell", approvalDecision{allow: true, all: true})
	if !cache.allowed("session-1", "shell") || !cache.allowed("session-1", "file_write") {
		t.Fatal("all-session approval did not allow every tool")
	}
	if cache.allowed("session-2", "shell") {
		t.Fatal("all-session approval leaked into another session")
	}
}
