#!/usr/bin/env bash
# Measures the chronos indexer's reference resolution against SCIP indexes of
# pinned repositories (docs/chronos-indexer.md, "Evaluation") and merges the
# results into benchmark/edges/baseline.json.
#
#   benchmark/edges/run.sh [name...]    # default: every repository in repos.tsv
#
# Needs git, go and curl, and per language:
#   TypeScript, Python  npx (the SCIP indexers are fetched into the npm cache)
#   Rust                rust-analyzer (rustup component add rust-analyzer)
#   Java, Kotlin        JDK 17 at $JAVA17_HOME (default: SDKMAN's
#                       17.0.18-tem); mvn for Maven projects without mvnw.
#                       scip-java is downloaded into $cache/tools.
#   C, C++              cmake and a C/C++ compiler; scip-clang (macOS arm64
#                       or Linux x86_64) is downloaded into $cache/tools.
#   C#                  the .NET SDK in $cache/dotnet, installed with
#                       dotnet-install.sh --channel 10.0 --install-dir
#                       $cache/dotnet; scip-dotnet is installed into
#                       $cache/tools. Its CLI state goes to $cache/dotnet-home.
# The reported numbers exclude the references listed in errata.tsv, and C/C++
# calls are checked against clang's AST (the raw SCIP numbers are kept under
# "raw"). Downloads are checked against the pinned SHA-256 sums. A repository whose
# toolchain is missing is skipped with a message. Checkouts, build
# directories and indexes are cached in $CHRONOS_EDGES_CACHE (default
# ~/.cache/chronos-edges).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
cache="${CHRONOS_EDGES_CACHE:-$HOME/.cache/chronos-edges}"
out="$here/baseline.json"
tags="$(make -s -C "$root" grammar-tags)"

SCIP_GO="github.com/scip-code/scip-go/cmd/scip-go@v0.2.7"
SCIP_TYPESCRIPT="@sourcegraph/scip-typescript@0.4.0"
SCIP_PYTHON="@sourcegraph/scip-python@0.6.6"
# v0.12.3, not 0.13: 0.13's javac wrapper expands an empty array under set -u,
# which macOS's bash 3.2 rejects.
SCIP_JAVA="v0.12.3"
SCIP_JAVA_SHA256="2d4d8a31333dfa0daf3aa0381a51de465e40b0dac5622e49363786a65f743f34"
SCIP_CLANG="v0.4.0"
SCIP_CLANG_SHA256_DARWIN_ARM64="ff042fbc8a029f09f4b69fc7692e290e21c52923593207ee52d4e7439473ec64"
SCIP_CLANG_SHA256_LINUX_X86_64="06fd18c576f979a726c651594644ec4a35db4f471f2160b3f72eb89fa6001784"
SCIP_DOTNET="0.2.14"
JAVA17="${JAVA17_HOME:-$HOME/.sdkman/candidates/java/17.0.18-tem}"

# fetch url sha256 file: downloads url to $cache/tools/file unless it is
# there, checking its SHA-256.
fetch() {
	local url="$1" sum="$2" file="$cache/tools/$3"
	[[ -x "$file" ]] && return 0
	mkdir -p "$cache/tools"
	echo "downloading $url" >&2
	curl -fsSL -o "$file.part" "$url" || return 1
	if [[ "$(shasum -a 256 "$file.part" | cut -d' ' -f1)" != "$sum" ]]; then
		echo "checksum mismatch for $url" >&2
		rm -f "$file.part"
		return 1
	fi
	chmod +x "$file.part"
	mv "$file.part" "$file"
}

mkdir -p "$cache"
while IFS=$'\t' read -r name lang url tag indexer args; do
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
	oracle=()
	start=$SECONDS
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
	scip-java) # Maven or Gradle, Java or Kotlin
		if [[ ! -x "$JAVA17/bin/java" ]]; then
			echo "skip $name: no JDK 17 at $JAVA17 (set JAVA17_HOME)" >&2
			continue
		fi
		if [[ -f "$dir/pom.xml" && ! -x "$dir/mvnw" ]] && ! command -v mvn >/dev/null; then
			echo "skip $name: mvn is not installed" >&2
			continue
		fi
		if ! fetch "https://github.com/sourcegraph/scip-java/releases/download/$SCIP_JAVA/scip-java-$SCIP_JAVA" \
			"$SCIP_JAVA_SHA256" "scip-java-$SCIP_JAVA"; then
			echo "skip $name: could not download scip-java $SCIP_JAVA" >&2
			continue
		fi
		tool="scip-java ${SCIP_JAVA#v}"
		(cd "$dir" && JAVA_HOME="$JAVA17" PATH="$JAVA17/bin:$PATH" "$cache/tools/scip-java-$SCIP_JAVA" index --output "$scip")
		;;
	scip-clang) # CMake projects; args are extra cmake options
		case "$(uname -sm)" in
		"Darwin arm64") asset="scip-clang-arm64-darwin" sum="$SCIP_CLANG_SHA256_DARWIN_ARM64" ;;
		"Linux x86_64") asset="scip-clang-x86_64-linux" sum="$SCIP_CLANG_SHA256_LINUX_X86_64" ;;
		*)
			echo "skip $name: no scip-clang release for $(uname -sm)" >&2
			continue
			;;
		esac
		if ! command -v cmake >/dev/null; then
			echo "skip $name: cmake is not installed" >&2
			continue
		fi
		if ! fetch "https://github.com/sourcegraph/scip-clang/releases/download/$SCIP_CLANG/$asset" \
			"$sum" "scip-clang-$SCIP_CLANG"; then
			echo "skip $name: could not download scip-clang $SCIP_CLANG" >&2
			continue
		fi
		tool="scip-clang ${SCIP_CLANG#v}"
		build="$cache/$name-build"
		# CMAKE_POLICY_VERSION_MINIMUM lets CMake 4 configure projects that
		# declare cmake_minimum_required below 3.5.
		# shellcheck disable=SC2086 # args holds several options
		cmake -S "$dir" -B "$build" -DCMAKE_EXPORT_COMPILE_COMMANDS=ON -DCMAKE_POLICY_VERSION_MINIMUM=3.5 $args >/dev/null
		(cd "$dir" && "$cache/tools/scip-clang-$SCIP_CLANG" --compdb-path="$build/compile_commands.json" --index-output-path="$scip")
		oracle=(-compdb "$build/compile_commands.json") # clang checks scip-clang's calls
		;;
	scip-dotnet) # args is the solution or project to index
		if [[ ! -x "$cache/dotnet/dotnet" ]]; then
			echo "skip $name: no .NET SDK in $cache/dotnet (dotnet-install.sh --channel 10.0 --install-dir $cache/dotnet)" >&2
			continue
		fi
		# XDG_DATA_HOME keeps NuGet's state out of ~/.local/share.
		dotnet_env=(DOTNET_ROOT="$cache/dotnet" DOTNET_CLI_HOME="$cache/dotnet-home" DOTNET_CLI_TELEMETRY_OPTOUT=1
			DOTNET_NOLOGO=1 DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 XDG_DATA_HOME="$cache/dotnet-home/.local/share"
			PATH="$cache/dotnet:$PATH")
		scip_dotnet="$cache/tools/scip-dotnet-$SCIP_DOTNET/scip-dotnet"
		if [[ ! -x "$scip_dotnet" ]] && ! env "${dotnet_env[@]}" dotnet tool install --tool-path "$cache/tools/scip-dotnet-$SCIP_DOTNET" \
			scip-dotnet --version "$SCIP_DOTNET" >&2; then
			echo "skip $name: could not install scip-dotnet $SCIP_DOTNET" >&2
			continue
		fi
		tool="scip-dotnet $SCIP_DOTNET (.NET SDK $(env "${dotnet_env[@]}" dotnet --version))"
		(cd "$dir" && env "${dotnet_env[@]}" "$scip_dotnet" index $args --output "$scip")
		;;
	*)
		echo "skip $name: unknown indexer $indexer" >&2
		continue
		;;
	esac
	echo "indexed $name in $((SECONDS - start))s" >&2
	echo "evaluating $name ($lang)" >&2
	go run -C "$root" -tags "$tags" ./benchmark/edges -name "$name" -lang "$lang" -repo "$dir" -scip "$scip" \
		-out "$out" -origin "$url" -commit "$commit" -indexer "$tool" -errata "$here/errata.tsv" ${oracle[@]+"${oracle[@]}"}
done <"$here/repos.tsv"
