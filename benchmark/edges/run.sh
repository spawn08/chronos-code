#!/usr/bin/env bash
# Measures the chronos indexer's reference resolution against SCIP indexes of
# pinned repositories (docs/chronos-indexer.md, "Evaluation") and merges the
# results into benchmark/edges/baseline.json.
#
#   benchmark/edges/run.sh [name...]    # default: every repository in repos.tsv
#
# Needs git and go; npx for TypeScript and Python (the SCIP indexers are
# fetched into the npm cache); rust-analyzer for Rust
# (rustup component add rust-analyzer). A repository whose indexer is
# missing is skipped with a message. Checkouts and indexes are cached in
# $CHRONOS_EDGES_CACHE (default ~/.cache/chronos-edges).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
cache="${CHRONOS_EDGES_CACHE:-$HOME/.cache/chronos-edges}"
out="$here/baseline.json"
tags="$(make -s -C "$root" grammar-tags)"

SCIP_GO="github.com/scip-code/scip-go/cmd/scip-go@v0.2.7"
SCIP_TYPESCRIPT="@sourcegraph/scip-typescript@0.4.0"
SCIP_PYTHON="@sourcegraph/scip-python@0.6.6"

mkdir -p "$cache"
while IFS=$'\t' read -r name lang url tag indexer; do
	[[ -z "$name" || "$name" == \#* ]] && continue
	if [[ $# -gt 0 ]] && [[ ! " $* " == *" $name "* ]]; then
		continue
	fi
	dir="$cache/$name"
	if [[ ! -d "$dir/.git" ]]; then
		git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$tag" "$url" "$dir"
	fi
	commit="$(git -C "$dir" rev-parse HEAD)"
	scip="$cache/$name.scip"
	case "$indexer" in
	scip-go)
		tool="scip-go ${SCIP_GO##*@}"
		(cd "$dir" && go run "$SCIP_GO" --output "$scip")
		;;
	scip-typescript)
		tool="scip-typescript ${SCIP_TYPESCRIPT##*@}"
		(cd "$dir" && npm install --ignore-scripts --no-audit --no-fund --silent && npx -y "$SCIP_TYPESCRIPT" index --output "$scip")
		;;
	scip-python)
		tool="scip-python ${SCIP_PYTHON##*@}"
		(cd "$dir" && npx -y "$SCIP_PYTHON" index . --project-name "$name" --output "$scip")
		;;
	rust-analyzer)
		if ! rust-analyzer --version >/dev/null 2>&1; then
			echo "skip $name: rust-analyzer is not installed (rustup component add rust-analyzer)" >&2
			continue
		fi
		tool="$(rust-analyzer --version)"
		(cd "$dir" && rust-analyzer scip . --output "$scip")
		;;
	*)
		echo "skip $name: unknown indexer $indexer" >&2
		continue
		;;
	esac
	echo "evaluating $name ($lang)" >&2
	go run -C "$root" -tags "$tags" ./benchmark/edges -name "$name" -lang "$lang" -repo "$dir" -scip "$scip" \
		-out "$out" -origin "$url" -commit "$commit" -indexer "$tool"
done <"$here/repos.tsv"
