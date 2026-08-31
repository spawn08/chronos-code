#!/usr/bin/env bash
# Evaluate autoresearch artifacts for completeness and quality
# Scores 0-100 based on: completeness, sophistication, efficiency, practicality

set -euo pipefail
ARTIFACTS_DIR="autoresearch/artifacts"
SCORE=0
MAX=100

echo "=== Autoresearch Evaluation ==="
echo ""

# 1. Check artifact existence (0-25 points)
REQUIRED_FILES=(
  "system-prompt-coder.yaml"
  "system-prompt-planner.yaml"
  "system-prompt-reviewer.yaml"
  "system-prompt-debugger.yaml"
  "system-prompt-researcher.yaml"
  "system-prompt-architect.yaml"
  "system-prompt-explainer.yaml"
  "default-skills.yaml"
  "loop-engineering.yaml"
  "graph-engineering.yaml"
  "mcp-configs.yaml"
  "enterprise-auth.yaml"
  "tool-definitions.yaml"
  "context-engineering.yaml"
  "agent-routing.yaml"
)

FOUND=0
for f in "${REQUIRED_FILES[@]}"; do
  if [[ -f "$ARTIFACTS_DIR/$f" ]]; then
    FOUND=$((FOUND + 1))
  else
    echo "MISSING: $f"
  fi
done

COMPLETENESS=$(( (FOUND * 25) / ${#REQUIRED_FILES[@]} ))
echo "Completeness: $FOUND/${#REQUIRED_FILES[@]} files = $COMPLETENESS/25"
SCORE=$((SCORE + COMPLETENESS))

# 2. Check sophistication — system prompts must be >500 chars (0-25 points)
SOPHISTICATED=0
TOTAL_PROMPTS=0
for f in "$ARTIFACTS_DIR"/system-prompt-*.yaml; do
  [[ -f "$f" ]] || continue
  TOTAL_PROMPTS=$((TOTAL_PROMPTS + 1))
  CHARS=$(wc -c < "$f")
  if [[ $CHARS -gt 2000 ]]; then
    SOPHISTICATED=$((SOPHISTICATED + 1))
  else
    echo "TOO_SHORT: $(basename "$f") ($CHARS chars, need >2000)"
  fi
done

if [[ $TOTAL_PROMPTS -gt 0 ]]; then
  SOPHISTICATION=$(( (SOPHISTICATED * 25) / TOTAL_PROMPTS ))
else
  SOPHISTICATION=0
fi
echo "Sophistication: $SOPHISTICATED/$TOTAL_PROMPTS prompts >2000 chars = $SOPHISTICATION/25"
SCORE=$((SCORE + SOPHISTICATION))

# 3. Check token efficiency — prompts should be <4000 chars (not bloated) (0-25 points)
EFFICIENT=0
for f in "$ARTIFACTS_DIR"/system-prompt-*.yaml; do
  [[ -f "$f" ]] || continue
  CHARS=$(wc -c < "$f")
  if [[ $CHARS -lt 15000 ]]; then
    EFFICIENT=$((EFFICIENT + 1))
  else
    echo "BLOATED: $(basename "$f") ($CHARS chars, want <15000)"
  fi
done

if [[ $TOTAL_PROMPTS -gt 0 ]]; then
  EFFICIENCY=$(( (EFFICIENT * 25) / TOTAL_PROMPTS ))
else
  EFFICIENCY=0
fi
echo "Efficiency: $EFFICIENT/$TOTAL_PROMPTS prompts <15000 chars = $EFFICIENCY/25"
SCORE=$((SCORE + EFFICIENCY))

# 4. Check practicality — YAML must be valid (0-25 points)
VALID=0
TOTAL_YAML=0
for f in "$ARTIFACTS_DIR"/*.yaml; do
  [[ -f "$f" ]] || continue
  TOTAL_YAML=$((TOTAL_YAML + 1))
  if python3 -c "import yaml; yaml.safe_load(open('$f'))" 2>/dev/null; then
    VALID=$((VALID + 1))
  else
    echo "INVALID_YAML: $(basename "$f")"
  fi
done

if [[ $TOTAL_YAML -gt 0 ]]; then
  PRACTICALITY=$(( (VALID * 25) / TOTAL_YAML ))
else
  PRACTICALITY=0
fi
echo "Practicality: $VALID/$TOTAL_YAML valid YAML = $PRACTICALITY/25"
SCORE=$((SCORE + PRACTICALITY))

echo ""
echo "=== TOTAL SCORE: $SCORE/$MAX ==="
echo "$SCORE"
