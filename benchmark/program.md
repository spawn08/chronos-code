# Autoresearch Program: Chronos Code — World-Class Coding Agent Harness

## Research Goal

Produce production-ready implementation artifacts for chronos-code — a world-class AI coding agent harness built on the Chronos framework. Each iteration produces or improves a concrete artifact (system prompt, skill definition, agent YAML, architecture design) that can be directly used by an agent harness to build chronos-code.

## Scope

1. **Sophisticated system prompts** for the primary agent and all subagents — prompts that produce expert-level output, not garbage
2. **Default skills** for software engineering tasks (code-review, debug, refactor, test, migrate, etc.)
3. **Loop engineering** — how agents should use iterative loops (plan→execute→verify→iterate)
4. **Graph engineering** — how agents should use the code graph for navigation, impact analysis, and context assembly
5. **Enterprise auth** — supporting enterprise models, API key models, and local models
6. **MCP integration** — default MCP server configs for common tools
7. **Tool definitions** — the minimal and extended tool sets
8. **Context engineering** — how to assemble minimal, high-signal context windows

## Evaluation Criteria (score 0-100)

- **Completeness** (0-25): Does the artifact cover all required capabilities?
- **Sophistication** (0-25): Are system prompts expert-level? Do they encode real SE knowledge?
- **Token efficiency** (0-25): Does the design minimize token consumption?
- **Practicality** (0-25): Can this be directly used in implementation without rewriting?

## Output Location

All artifacts go into `autoresearch/artifacts/` as YAML files.

## Iteration Strategy

1. Start with the primary coding agent's system prompt (highest impact)
2. Then subagent prompts (planner, reviewer, debugger, researcher, architect)
3. Then default skills with manifests
4. Then loop/graph engineering patterns
5. Then MCP/auth/tool configs
6. Each iteration improves the weakest artifact or adds a missing one
