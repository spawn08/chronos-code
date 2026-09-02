package learning

import (
	"reflect"
	"testing"
	"time"
)

func TestNormalizeTrigger(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  string
	}{
		{"  Fix   THE bug!  ", "fix the bug"},
		{"Add_test-for foo.Bar", "add test for foo bar"},
		{"already normalized", "already normalized"},
	} {
		if got := NormalizeTrigger(tt.input); got != tt.want {
			t.Errorf("NormalizeTrigger(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestClusterCandidatesUsesExactNormalizedGroupsAndStableOrder(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	var segments []SessionSegment
	for i, trigger := range []string{"Fix bug", " fix  BUG! ", "FIX-BUG"} {
		segments = append(segments, testSegment("/repo", trigger, "accepted", base.Add(time.Duration(i)*time.Minute), "read", "test"))
	}
	for i := 0; i < 3; i++ {
		segments = append(segments, testSegment("/repo", "add docs", "accepted", base.Add(time.Duration(i)*time.Minute), "read"))
	}

	got := ClusterCandidates(segments, MinimumCandidateCount)
	if len(got) != 2 {
		t.Fatalf("ClusterCandidates() returned %d candidates, want 2", len(got))
	}
	if got[0].TriggerHash > got[1].TriggerHash {
		t.Errorf("candidate hashes are not ordered: %q, %q", got[0].TriggerHash, got[1].TriggerHash)
	}
	for _, candidate := range got {
		if candidate.SuccessCount != 3 || candidate.FailCount != 0 {
			t.Errorf("candidate counts = %d/%d, want 3/0", candidate.SuccessCount, candidate.FailCount)
		}
	}
	if got[0].TriggerHash == got[1].TriggerHash {
		t.Fatal("different normalized triggers were grouped together")
	}
}

func TestCandidateThresholdIsStrictlyGreaterThanSeventyPercent(t *testing.T) {
	var segments []SessionSegment
	for i := 0; i < 10; i++ {
		kind := "accepted"
		if i >= 7 {
			kind = "rejected"
		}
		segments = append(segments, testSegment("/repo", "exactly seventy", kind, time.Unix(int64(i), 0), "test"))
	}
	if got := ClusterCandidates(segments, MinimumCandidateCount); len(got) != 0 {
		t.Fatalf("ClusterCandidates() at 70%% returned %d candidates, want 0", len(got))
	}

	segments[7].Outcome.Kind = "accepted"
	got := ClusterCandidates(segments, MinimumCandidateCount)
	if len(got) != 1 || got[0].SuccessCount != 8 || got[0].FailCount != 2 {
		t.Fatalf("ClusterCandidates() above 70%% = %+v, want one 8/2 candidate", got)
	}

	tooFew := segments[:MinimumCandidateCount-1]
	if got := ClusterCandidates(tooFew, MinimumCandidateCount); len(got) != 0 {
		t.Fatalf("ClusterCandidates() below minimum count returned %d candidates, want 0", len(got))
	}
}

func TestClusterCandidatesSelectsToolSequenceDeterministically(t *testing.T) {
	base := time.Unix(0, 0)
	segments := []SessionSegment{
		testSegment("/repo", "fix bug", "accepted", base, "write", "test"),
		testSegment("/repo", "fix bug", "accepted", base.Add(time.Second), "read", "test"),
		testSegment("/repo", "fix bug", "accepted", base.Add(2*time.Second), "write", "test"),
	}
	want := []string{"write", "test"}
	got := ClusterCandidates(segments, MinimumCandidateCount)
	if len(got) != 1 || !reflect.DeepEqual(got[0].ToolSequence, want) {
		t.Fatalf("ClusterCandidates().ToolSequence = %v, want %v", got, want)
	}
}

func testSegment(repoPath, trigger, outcome string, at time.Time, tools ...string) SessionSegment {
	calls := make([]ToolCall, len(tools))
	for i, name := range tools {
		calls[i] = ToolCall{Name: name}
	}
	return SessionSegment{
		RepoPath:  repoPath,
		Trigger:   Turn{Content: trigger},
		Turns:     []Turn{{Role: "user", Content: trigger}, {Role: "assistant", Content: "completed " + trigger}},
		ToolCalls: calls,
		Outcome:   &Outcome{Kind: outcome, Timestamp: at},
	}
}
