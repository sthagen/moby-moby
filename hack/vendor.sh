#!/usr/bin/env bash
#
# This file is just a wrapper around the 'go mod vendor' tool.
# For updating dependencies you should change `go.mod` file in root of the
# project.

set -e

SCRIPTDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPTDIR/.." && pwd)"

# All module paths are relative to PROJECT_DIR
# Currently only a subset of modules are vendored.
vendor_modules_paths=(. man)
modules_paths=(api client "${vendor_modules_paths[@]}")

tidy() (
	for module_path in "${modules_paths[@]}"; do
		(
			set -x
			cd "$PROJECT_DIR/$module_path"
			go mod tidy
		)
	done
)

vendor() (
	for module_path in "${vendor_modules_paths[@]}"; do
		(
			set -x
			cd "$PROJECT_DIR/$module_path"
			go mod vendor
		)
	done
)

replace() (
	set -x
	go mod edit -replace=github.com/moby/moby/api=./api -replace=github.com/moby/moby/client=./client
	go mod edit -modfile client/go.mod -replace=github.com/moby/moby/api=../api
)

dropreplace() (
	set -x
	go mod edit -dropreplace=github.com/moby/moby/api -dropreplace=github.com/moby/moby/client
	go mod edit -modfile client/go.mod -dropreplace=github.com/moby/moby/api

	go mod edit -modfile client/go.mod -require='github.com/moby/moby/api@master'
	(cd client; go mod tidy)

	go mod edit \
		-require='github.com/moby/moby/api@master' \
		-require='github.com/moby/moby/client@master'
	go mod tidy
	go mod vendor
)

help() {
	printf "%s:\n" "$(basename "$0")"
	echo "  - tidy: run go mod tidy"
	echo "  - vendor: run go mod vendor"
	echo "  - replace: run go mod edit replace for local modules"
	echo "  - dropreplace: run go mod edit dropreplace for local modules"
	echo "  - all: run tidy && vendor"
	echo "  - help: show this help"
}

case "$1" in
	tidy) tidy ;;
	vendor) vendor ;;
	replace) replace ;;
	dropreplace) dropreplace ;;
	""|all) tidy && vendor ;;
	*) help ;;
esac
