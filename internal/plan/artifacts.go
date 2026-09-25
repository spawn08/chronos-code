package plan

import (
	"fmt"
	"sort"
)

// AcceptedArtifactIDs returns the accepted patches of a node's transitive
// predecessors in dependency order. No ambient parent HEAD changes enter the
// private workspace through this path.
func (p Plan) AcceptedArtifactIDs(nodeID NodeID) ([]string, error) {
	states := make(map[NodeID]NodeState, len(p.Nodes))
	for _, node := range p.Nodes {
		states[node.ID] = node.State
	}
	if _, ok := states[nodeID]; !ok {
		return nil, fmt.Errorf("resolve predecessor artifacts: unknown node %q", nodeID)
	}
	dependencies := make(map[NodeID][]NodeID, len(p.Nodes))
	for _, edge := range p.Dependencies {
		dependencies[edge.NodeID] = append(dependencies[edge.NodeID], edge.DependsOn)
	}
	artifacts := make(map[NodeID]string, len(p.Artifacts))
	for _, artifact := range p.Artifacts {
		if artifact.Undone {
			continue
		}
		if _, ok := states[artifact.NodeID]; !ok || artifact.ID == "" || artifacts[artifact.NodeID] != "" {
			return nil, fmt.Errorf("resolve predecessor artifacts: invalid accepted artifact for %q", artifact.NodeID)
		}
		artifacts[artifact.NodeID] = artifact.ID
	}
	visited := make(map[NodeID]bool)
	visiting := make(map[NodeID]bool)
	var result []string
	var visit func(NodeID) error
	visit = func(id NodeID) error {
		if visiting[id] {
			return ErrInvalidDAG
		}
		if visited[id] {
			return nil
		}
		if states[id] != NodeCompleted {
			return fmt.Errorf("resolve predecessor artifacts: node %q is not completed", id)
		}
		visiting[id] = true
		parents := append([]NodeID(nil), dependencies[id]...)
		sort.Slice(parents, func(i, j int) bool { return parents[i] < parents[j] })
		for _, parent := range parents {
			if err := visit(parent); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		if artifact := artifacts[id]; artifact != "" {
			result = append(result, artifact)
		}
		return nil
	}
	parents := append([]NodeID(nil), dependencies[nodeID]...)
	sort.Slice(parents, func(i, j int) bool { return parents[i] < parents[j] })
	for _, parent := range parents {
		if err := visit(parent); err != nil {
			return nil, err
		}
	}
	return result, nil
}
