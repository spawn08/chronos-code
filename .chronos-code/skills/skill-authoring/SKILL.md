---
name: skill-authoring
description: How to author SKILL.md files for chronos-code — frontmatter schema, discovery, selection
version: 1.0.0
triggers: [skill, SKILL.md, author, create skill, add skill, trigger, manifest]
model_hint: sonnet
tools_required: [file_read, file_write, shell]
---
# Skill Authoring for Chronos Code

## SKILL.md Format
Every skill is a directory containing a `SKILL.md` file with YAML frontmatter:

```markdown
---
name: my-skill
description: One-line description of when to use this skill
version: 1.0.0
triggers: [keyword1, keyword2, phrase]
model_hint: sonnet        # optional: routing hint for model selection
tools_required: [file_read, file_write, shell]
---
# Skill Body (markdown)

Instructions the agent reads when this skill is selected.
```

## Required Fields
- `name` — unique, lowercase, hyphenated (MUST be present or parse fails)
- `description` — used for BM25 matching alongside triggers
- `triggers` — keyword list; these are the primary matching signal

## Discovery Locations (highest priority wins)
1. `<repo>/.chronos-code/skills/<name>/SKILL.md` — project-specific
2. `<repo>/.<provider>/skills/<name>/SKILL.md` — provider directories (.claude, .codex, .gemini, .opencode, .cursor, .windsurf, .github, .agents)
3. `~/.chronos-code/skills/<name>/SKILL.md` — user global
4. `~/.<provider>/skills/<name>/SKILL.md` — user provider dirs
5. `~/.chronos-code/plugins/<plugin>/skills/<name>/SKILL.md` — plugins
6. Bundled defaults (embedded in binary)

## Selection Algorithm
- BM25 scoring over `name + description + triggers` against user message + recent tool history
- Top-K=3 skills selected per turn (configurable)
- Token budget: 8000 tokens total for all selected skills
- Lowest-scored skill dropped first if over budget
- Rendered as `<skill name="...">body</skill>` XML blocks in system prompt

## Cross-Provider Compatibility
Skills placed in `.claude/skills/`, `.codex/skills/`, etc. are automatically discovered. This lets teams share skills across Claude Code, Codex, and chronos-code.

## Self-Learning Promotion
The `learn promote` CLI command creates a SKILL.md from a learned pattern:
- Stored in `~/.chronos-code/skills/learned-<hash>/SKILL.md`
- User reviews before accepting

## Best Practices
- Keep trigger lists focused (5-10 keywords); too many dilute BM25 scores
- Body should be actionable instructions, not general knowledge
- Include tool names the agent should prefer
- Keep body under 2000 tokens to leave room for other skills
- Test selection: run `/skills` in the TUI to see what gets discovered
