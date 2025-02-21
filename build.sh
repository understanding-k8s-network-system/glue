#!/usr/bin/env bash
set -e
cd "$(dirname "$0")"

export GOFLAGS="${GOFLAGS} -mod=vendor"

[ ! -d ${PWD}/bin ] && mkdir -p "${PWD}/bin"

echo "Building..."
APPS="cni-plugins/* glued"
for d in $APPS; do
	if [ -d "$d" ]; then
		app="$(basename "$d")"
		echo "Build $app"
		${GO:-go} build -o "${PWD}/bin/$app" "$@" ./"$d"
	fi
done
