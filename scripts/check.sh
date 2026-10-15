#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export GOWORK=off
test -z "$(gofmt -l cmd internal payment tdcloud)"
go test -race ./...
go vet ./...
node --test ui/*.test.mjs
python3 scripts/release.py check
python3 -m unittest discover -s scripts -p '*_test.py'
git diff --check
