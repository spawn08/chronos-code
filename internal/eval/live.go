package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/engine/tool"
)

// LiveExecutor runs the actual orchestrator in TaskRunner's isolated checkout.
// The factory supplies a fresh resolved configuration per run. Hidden graders
// remain outside the agent; a confident final answer alone cannot pass them.
func LiveExecutor(factory func() (*config.Config, error)) EnvelopeExecutor {
	return func(ctx context.Context, task TaskExecution) (EnvelopeRecord, error) {
		cfg, err := factory()
		if err != nil {
			return EnvelopeRecord{}, err
		}
		if task.Ablation.Worktrees {
			return EnvelopeRecord{}, fmt.Errorf("live evaluator: worktree ablation is not supported by this adapter")
		}
		if len(task.Environment) != 0 {
			return EnvelopeRecord{}, fmt.Errorf("live evaluator: environment overrides require a subprocess adapter")
		}
		cfg.Workspace.Root = task.Workspace
		cfg.Ephemeral = true
		cfg.Memory.Enabled = false
		no := false
		cfg.Session.RecallPriorSummaries = &no
		cfg.MCP.Discovery = &no
		cfg.MCP.Strict = true
		cfg.Workspace.IndexOnStart = &task.Ablation.Graph
		cfg.SkillSources = "project,bundled"
		var effects []tool.Effect
		permissions := map[string]bool{}
		for _, permission := range task.Permissions {
			permissions[permission] = true
			switch permission {
			case "read":
				effects = append(effects, tool.EffectRead)
			case "write":
				effects = append(effects, tool.EffectDeliveryWrite, tool.EffectScratchWrite)
			case "shell":
				effects = append(effects, tool.EffectProcessExecution)
			case "network":
				effects = append(effects, tool.EffectNetwork, tool.EffectExternalMutation)
			}
		}
		if !permissions["network"] {
			for i := range cfg.Agents {
				cfg.Agents[i].MCPServers = nil
			}
		}
		orch, err := orchestrator.New(ctx, cfg, "")
		if err != nil {
			return EnvelopeRecord{}, err
		}
		defer orch.Close()
		if err := orch.SetPermissionMode("auto_approve"); err != nil {
			return EnvelopeRecord{}, err
		}
		if !task.Ablation.Specialists {
			orch.DisableTools("spawn_subagent")
		}
		if !task.Ablation.Skills {
			orch.DisableTools("skill")
		}
		if !task.Ablation.Graph {
			orch.DisableTools("codebase_search", "codebase_context", "codebase_map", "resolve_symbol", "graph_query", "find_callers", "find_implementations", "impact_analysis", "test_map", "co_change", "multi_resolution_view")
		}
		ctx = tool.WithEffectGrant(ctx, effects...)
		result, executeErr := orch.Execute(ctx, orchestrator.ExecutionRequest{Message: task.Instructions, VerificationMode: verification.ModeReport, DisableRepair: !task.Ablation.Repair})
		record := EnvelopeRecord{Envelope: orchestrator.ExecutionEnvelope(result, executeErr, orchestrator.EnvelopeMetadata{})}
		// Each real trace is counted once; preserve billed usage classes rather
		// than deriving model rounds from a synthetic terminal envelope.
		traces, traceErr := orch.SessionTraces(context.WithoutCancel(ctx), result.SessionID)
		if traceErr != nil {
			return record, traceErr
		}
		for _, trace := range traces {
			call := Call{ID: trace.ID, Duration: trace.EndedAt.Sub(trace.StartedAt)}
			if trace.EndedAt.IsZero() {
				call.Duration = time.Since(trace.StartedAt)
			}
			switch trace.Kind {
			case "tool_call":
				call.Kind = CallTool
			case "model_call":
				call.Kind = CallModel
				data, err := json.Marshal(trace.Output)
				if err != nil {
					return record, err
				}
				var output struct {
					Usage struct {
						Input  int `json:"input_tokens"`
						Output int `json:"output_tokens"`
						Read   int `json:"cache_read_tokens"`
						Write  int `json:"cache_creation_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(data, &output); err != nil {
					return record, err
				}
				call.Usage = Usage{InputTokens: output.Usage.Input, OutputTokens: output.Usage.Output, CacheTokens: output.Usage.Read + output.Usage.Write, CacheReadTokens: output.Usage.Read, CacheWriteTokens: output.Usage.Write}
			case "request_context":
				record.RequestContexts = append(record.RequestContexts, trace.Output)
				continue
			default:
				continue
			}
			record.Calls = append(record.Calls, call)
		}
		return record, nil
	}
}
