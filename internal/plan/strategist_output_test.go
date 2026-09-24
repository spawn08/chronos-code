package plan

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseStrategistOutputAcceptsExactJSON(t *testing.T) {
	output, err := ParseStrategistOutput([]byte(validStrategistOutputJSON))
	if err != nil {
		t.Fatal(err)
	}
	if output.SourceRequestRef != "request-1" || output.ClassifierRef != "classifier-1" || len(output.Nodes) != 2 {
		t.Fatalf("strategist output = %#v", output)
	}
	if output.Nodes[1].ID != "verify" || output.Nodes[1].Kind != NodeVerify || output.Nodes[1].DependsOn[0] != "implement" || output.Nodes[1].ExpectedArtifacts[0] != "race-test result" {
		t.Fatalf("strategist nodes = %#v", output.Nodes)
	}
}

func TestParseStrategistOutputRejectsInvalidJSONSchemaAndGraph(t *testing.T) {
	tooManyNodes := make([]string, MaxDecompositionNodes+1)
	for i := range tooManyNodes {
		tooManyNodes[i] = fmt.Sprintf(`{"id":"node-%d","kind":"investigate","objective":"find evidence","depends_on":[],"scope":"scope","context_refs":[],"expected_artifacts":["finding"],"assumptions":[],"invalidation_triggers":[],"recovery_class":"replan","risks":["risk"],"verification":"check"}`, i)
	}
	tests := []struct {
		name string
		json string
	}{
		{"prose wrapper", "Here is the plan: " + validStrategistOutputJSON},
		{"markdown wrapper", "```json\n" + validStrategistOutputJSON + "\n```"},
		{"trailing content", validStrategistOutputJSON + " trailing"},
		{"unknown root field", strings.Replace(validStrategistOutputJSON, `"nodes":`, `"unknown":true,"nodes":`, 1)},
		{"unknown node field", strings.Replace(validStrategistOutputJSON, `"id":"implement"`, `"id":"implement","unknown":true`, 1)},
		{"missing source reference", strings.Replace(validStrategistOutputJSON, `"source_request_ref":"request-1",`, "", 1)},
		{"missing classifier reference", strings.Replace(validStrategistOutputJSON, `"classifier_ref":"classifier-1",`, "", 1)},
		{"missing node ID", strings.Replace(validStrategistOutputJSON, `"id":"implement",`, "", 1)},
		{"missing kind", strings.Replace(validStrategistOutputJSON, `"kind":"implement",`, "", 1)},
		{"unknown kind", strings.Replace(validStrategistOutputJSON, `"kind":"implement"`, `"kind":"write"`, 1)},
		{"missing objective", strings.Replace(validStrategistOutputJSON, `"objective":"implement the parser contract",`, "", 1)},
		{"oversized objective", strings.Replace(validStrategistOutputJSON, `implement the parser contract`, strings.Repeat("x", maxNodeTextBytes+1), 1)},
		{"missing dependencies", strings.Replace(validStrategistOutputJSON, `"depends_on":[],`, "", 1)},
		{"missing scope", strings.Replace(validStrategistOutputJSON, `"scope":"internal/plan",`, "", 1)},
		{"missing context refs", strings.Replace(validStrategistOutputJSON, `"context_refs":["graph-plan"],`, "", 1)},
		{"missing expected artifacts", strings.Replace(validStrategistOutputJSON, `"expected_artifacts":["parser implementation"],`, "", 1)},
		{"missing assumptions", strings.Replace(validStrategistOutputJSON, `"assumptions":[],`, "", 1)},
		{"missing invalidation triggers", strings.Replace(validStrategistOutputJSON, `"invalidation_triggers":["schema changes"],`, "", 1)},
		{"invalid recovery class", strings.Replace(validStrategistOutputJSON, `"recovery_class":"replan"`, `"recovery_class":"ignore"`, 1)},
		{"missing risks", strings.Replace(validStrategistOutputJSON, `"risks":["invalid graph"],`, "", 1)},
		{"missing verification", strings.Replace(validStrategistOutputJSON, `,"verification":"go test ./internal/plan"`, "", 1)},
		{"duplicate node IDs", strings.Replace(validStrategistOutputJSON, `"id":"verify"`, `"id":"implement"`, 1)},
		{"duplicate edge", strings.Replace(validStrategistOutputJSON, `"depends_on":["implement"]`, `"depends_on":["implement","implement"]`, 1)},
		{"duplicate context refs", strings.Replace(validStrategistOutputJSON, `"context_refs":["test-plan"]`, `"context_refs":["graph-plan"]`, 1)},
		{"oversized context ref", strings.Replace(validStrategistOutputJSON, `graph-plan`, strings.Repeat("x", maxContextIDBytes+1), 1)},
		{"dangling dependency", strings.Replace(validStrategistOutputJSON, `"depends_on":["implement"]`, `"depends_on":["missing"]`, 1)},
		{"dependency cycle", strings.Replace(validStrategistOutputJSON, `"depends_on":[]`, `"depends_on":["verify"]`, 1)},
		{"more than maximum nodes", fmt.Sprintf(`{"source_request_ref":"request-1","classifier_ref":"classifier-1","nodes":[%s]}`, strings.Join(tooManyNodes, ","))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseStrategistOutput([]byte(test.json)); !errors.Is(err, ErrInvalidDecomposition) {
				t.Fatalf("ParseStrategistOutput() error = %v, want %v", err, ErrInvalidDecomposition)
			}
		})
	}
}

const validStrategistOutputJSON = `{"source_request_ref":"request-1","classifier_ref":"classifier-1","nodes":[{"id":"implement","kind":"implement","objective":"implement the parser contract","depends_on":[],"scope":"internal/plan","context_refs":["graph-plan"],"expected_artifacts":["parser implementation"],"assumptions":[],"invalidation_triggers":["schema changes"],"recovery_class":"replan","risks":["invalid graph"],"verification":"go test ./internal/plan"},{"id":"verify","kind":"verify","objective":"verify the strict contract","depends_on":["implement"],"scope":"internal/plan tests","context_refs":["test-plan"],"expected_artifacts":["race-test result"],"assumptions":["implementation is available"],"invalidation_triggers":[],"recovery_class":"retry","risks":["missing regression"],"verification":"go test ./internal/plan"}]}`
