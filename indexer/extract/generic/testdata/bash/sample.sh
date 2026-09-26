#!/usr/bin/env bash
source ./lib/common.sh
. "$HOME/.profile"

readonly VERSION="1.0"
export LOG_LEVEL=info

# Prints usage.
usage() {
  echo "usage: $0"
}

function build {
  log_info "building"
  make -C src all
  usage
}

main() {
  build "$@"
}

main "$@"
