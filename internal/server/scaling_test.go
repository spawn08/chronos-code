package server

import "testing"

func TestSessionRouter_ClaimAndIsLocal(t *testing.T) {
	r := NewSessionRouter("inst-1")
	r.Claim("sess-a")

	if !r.IsLocal("sess-a") {
		t.Fatal("expected sess-a to be local after claim")
	}
}

func TestSessionRouter_Owner(t *testing.T) {
	r := NewSessionRouter("inst-1")
	r.Claim("sess-a")

	if got := r.Owner("sess-a"); got != "inst-1" {
		t.Fatalf("owner = %q, want inst-1", got)
	}
}

func TestSessionRouter_Release(t *testing.T) {
	r := NewSessionRouter("inst-1")
	r.Claim("sess-a")
	r.Release("sess-a")

	if r.IsLocal("sess-a") {
		t.Fatal("expected sess-a to not be local after release")
	}
	if got := r.Owner("sess-a"); got != "" {
		t.Fatalf("owner after release = %q, want empty", got)
	}
}

func TestSessionRouter_UnclaimedOwner(t *testing.T) {
	r := NewSessionRouter("inst-1")

	if got := r.Owner("nonexistent"); got != "" {
		t.Fatalf("unclaimed owner = %q, want empty", got)
	}
	if r.IsLocal("nonexistent") {
		t.Fatal("unclaimed session should not be local")
	}
}

func TestSessionRouter_MultipleSessions(t *testing.T) {
	r := NewSessionRouter("inst-1")
	r.Claim("sess-a")
	r.Claim("sess-b")

	if !r.IsLocal("sess-a") || !r.IsLocal("sess-b") {
		t.Fatal("expected both sessions to be local")
	}

	r.Release("sess-a")
	if r.IsLocal("sess-a") {
		t.Fatal("sess-a should not be local after release")
	}
	if !r.IsLocal("sess-b") {
		t.Fatal("sess-b should still be local")
	}
}

func TestSessionRouter_InstanceID(t *testing.T) {
	r := NewSessionRouter("my-instance")
	if r.InstanceID() != "my-instance" {
		t.Fatalf("instance id = %q, want my-instance", r.InstanceID())
	}
}

func TestSessionRouter_AutoGenerateInstanceID(t *testing.T) {
	r := NewSessionRouter("")
	if r.InstanceID() == "" {
		t.Fatal("auto-generated instance id should not be empty")
	}
}
