#!/usr/bin/env bash
# Measures whether a model follows an operator rule placed at the end of a
# ~16 KB brief, delivered two ways: as the repository AGENTS.md, and as
# --system-prompt-file. The rule: end every reply with BANANA-42.
#
# Usage:
#   scripts/eval-operator-instructions.sh [model ...]
#
# Env overrides:
#   CHRONOS_CODE_BIN  binary to test (default: bin/chronos-code)
#   TRIALS            runs per model and delivery (default: 3)
#
# Needs provider credentials for each model and jq. Prints one TSV row per
# model and delivery: model, delivery, passed, trials.

set -euo pipefail

BIN="${CHRONOS_CODE_BIN:-bin/chronos-code}"
TRIALS="${TRIALS:-3}"
TOKEN="BANANA-42"
if [ "$#" -eq 0 ]; then
  set -- anthropic/claude-sonnet-4-5 anthropic/claude-haiku-4-5
fi
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"
command -v jq >/dev/null || { echo "error: jq is required" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

brief="$work/brief.md"
{
  echo "# Runtime brief"
  echo
  for i in $(seq 1 53); do
    echo "## Section $i: working rules"
    echo
    echo "Read the issue before you change code. Keep each change small and explain it in the issue comment. Run only the checks the issue names. Do not edit generated files. Keep the commit message short and name the issue. When a step fails, report the error text in the issue and stop."
    echo
  done
  echo "## Reply format"
  echo
  echo "End every reply with the exact token $TOKEN on its own line."
} >"$brief"

run_case() {
  local model="$1" delivery="$2" repo="$work/repo-$delivery"
  rm -rf "$repo" && mkdir -p "$repo"
  git -C "$repo" init -q
  echo "A small service that returns the current time." >"$repo/README.md"
  local extra=()
  if [ "$delivery" = agents-md ]; then
    cp "$brief" "$repo/AGENTS.md"
  else
    extra=(--system-prompt-file "$brief")
  fi
  (cd "$repo" && echo "In one sentence, what is this repository for?" |
    CHRONOS_CODE_DATA_HOME="$work/data" CHRONOS_CODE_EPHEMERAL=1 "$BIN" run --model "$model" \
      --output-format stream-json --prompt-stdin --strict-mcp-config --skill-sources project \
      --max-turns 4 ${extra[@]+"${extra[@]}"} 2>/dev/null) |
    jq -r 'select(.type == "completion") | .payload.content' || true
}

for model in "$@"; do
  for delivery in agents-md system-prompt-file; do
    passed=0
    for _ in $(seq 1 "$TRIALS"); do
      last="$(run_case "$model" "$delivery" | sed -e 's/[[:space:]]*$//' | awk 'NF { line = $0 } END { print line }')"
      case "$last" in
      *"$TOKEN") passed=$((passed + 1)) ;;
      esac
    done
    printf '%s\t%s\t%d\t%d\n' "$model" "$delivery" "$passed" "$TRIALS"
  done
done
