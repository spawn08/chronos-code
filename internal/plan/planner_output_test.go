package plan

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParsePlannerOutputAcceptsExactJSON(t *testing.T) {
	output, err := ParsePlannerOutput([]byte(validPlannerOutputJSON))
	if err != nil {
		t.Fatal(err)
	}
	if output.SourceRequestRef != "request-1" || output.ClassifierRef != "classifier-1" || len(output.Nodes) != 2 {
		t.Fatalf("planner output = %#v", output)
	}
	if output.Nodes[1].ID != "verify" || output.Nodes[1].DependsOn[0] != "implement" || output.Nodes[1].Verification == "" {
		t.Fatalf("planner nodes = %#v", output.Nodes)
	}
}

func TestParsePlannerOutputRejectsInvalidJSONSchemaAndGraph(t *testing.T) {
	tooManyNodes := make([]string, MaxDecompositionNodes+1)
	for i := range tooManyNodes {
		tooManyNodes[i] = fmt.Sprintf(`{"id":"node-%d","depends_on":[],"scope":"scope","context_refs":[],"risks":["risk"],"verification":"check"}`, i)
	}
	tests := []struct {
		name string
		json string
	}{
		{"prose wrapper", "Here is the plan: " + validPlannerOutputJSON},
		{"markdown wrapper", "```json\n" + validPlannerOutputJSON + "\n```"},
		{"trailing content", validPlannerOutputJSON + " trailing"},
		{"unknown root field", strings.Replace(validPlannerOutputJSON, `"nodes":`, `"unknown":true,"nodes":`, 1)},
		{"unknown node field", strings.Replace(validPlannerOutputJSON, `"id":"implement"`, `"id":"implement","unknown":true`, 1)},
		{"missing source reference", strings.Replace(validPlannerOutputJSON, `"source_request_ref":"request-1",`, "", 1)},
		{"missing classifier reference", strings.Replace(validPlannerOutputJSON, `"classifier_ref":"classifier-1",`, "", 1)},
		{"missing node ID", strings.Replace(validPlannerOutputJSON, `"id":"implement",`, "", 1)},
		{"missing dependencies", strings.Replace(validPlannerOutputJSON, `"depends_on":[],`, "", 1)},
		{"missing scope", strings.Replace(validPlannerOutputJSON, `"scope":"internal/plan",`, "", 1)},
		{"missing context refs", strings.Replace(validPlannerOutputJSON, `"context_refs":["graph-plan"],`, "", 1)},
		{"missing risks", strings.Replace(validPlannerOutputJSON, `"risks":["invalid graph"],`, "", 1)},
		{"missing verification", strings.Replace(validPlannerOutputJSON, `,"verification":"go test ./internal/plan"`, "", 1)},
		{"duplicate node IDs", strings.Replace(validPlannerOutputJSON, `"id":"verify"`, `"id":"implement"`, 1)},
		{"duplicate edge", strings.Replace(validPlannerOutputJSON, `"depends_on":["implement"]`, `"depends_on":["implement","implement"]`, 1)},
		{"duplicate context refs", strings.Replace(validPlannerOutputJSON, `"context_refs":["test-plan"]`, `"context_refs":["graph-plan"]`, 1)},
		{"dangling dependency", strings.Replace(validPlannerOutputJSON, `"depends_on":["implement"]`, `"depends_on":["missing"]`, 1)},
		{"dependency cycle", strings.Replace(validPlannerOutputJSON, `"depends_on":[]`, `"depends_on":["verify"]`, 1)},
		{"more than maximum nodes", fmt.Sprintf(`{"source_request_ref":"request-1","classifier_ref":"classifier-1","nodes":[%s]}`, strings.Join(tooManyNodes, ","))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParsePlannerOutput([]byte(test.json)); !errors.Is(err, ErrInvalidDecomposition) {
				t.Fatalf("ParsePlannerOutput() error = %v, want %v", err, ErrInvalidDecomposition)
			}
		})
	}
}

const validPlannerOutputJSON = `{"source_request_ref":"request-1","classifier_ref":"classifier-1","nodes":[{"id":"implement","depends_on":[],"scope":"internal/plan","context_refs":["graph-plan"],"risks":["invalid graph"],"verification":"go test ./internal/plan"},{"id":"verify","depends_on":["implement"],"scope":"internal/plan tests","context_refs":["test-plan"],"risks":["missing regression"],"verification":"go test ./internal/plan"}]}`
