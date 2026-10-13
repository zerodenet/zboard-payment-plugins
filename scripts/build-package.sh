#!/bin/sh
set -eu

if [ "$#" -ne 1 ] && [ "$#" -ne 2 ] && [ "$#" -ne 4 ]; then
  echo "usage: $0 /absolute/output.zbplugin [all|platform]" >&2
  echo "       $0 /absolute/output.zbplugin /absolute/private-key-file publisher-id all|platform" >&2
  exit 2
fi

output=$1
key=
publisher=
target=${2:-all}
if [ "$#" -eq 4 ]; then
  key=$2
  publisher=$3
  target=$4
fi
case "$target" in
  all|linux-amd64|linux-arm64|darwin-amd64|darwin-arm64) ;;
  *) echo "unsupported target: $target" >&2; exit 2 ;;
esac
case "$output" in
  /*) ;;
  *) echo "output path must be absolute" >&2; exit 2 ;;
esac
if [ -n "$key" ]; then
  case "$key" in /*) ;; *) echo "key path must be absolute" >&2; exit 2 ;; esac
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
board_backend=$(CDPATH= cd -- "$root/../zboard/backend" && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
python3 - "$root/manifest.json" "$stage/manifest.json" "$target" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    manifest = json.load(source)
target = sys.argv[3]
executables = manifest["components"]["server"]["executables"]
if target != "all":
    manifest["components"]["server"]["executables"] = {target: executables[target]}
with open(sys.argv[2], "w", encoding="utf-8") as output_file:
    json.dump(manifest, output_file, ensure_ascii=False)
PY
targets=$target
if [ "$target" = all ]; then
  targets="linux-amd64 linux-arm64 darwin-amd64 darwin-arm64"
fi
for platform in $targets; do
  os=${platform%-*}
  arch=${platform#*-}
  mkdir -p "$stage/runtimes/$platform"
  (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags='-s -w' -o "$stage/runtimes/$platform/payments" ./cmd/payments)
done
mkdir -p "$stage/ui"
cp "$root/ui/payment.html" "$root/ui/payment-entry.mjs" "$root/ui/plugin-bridge.mjs" "$root/ui/payment-flow.mjs" "$root/ui/payment-widget.mjs" "$root/ui/config.html" "$root/ui/config-entry.mjs" "$root/ui/channel-config-providers.mjs" "$root/ui/tdcloud-config.mjs" "$stage/ui/"

mkdir -p "$(dirname -- "$output")"
if [ -n "$key" ]; then
  (cd "$board_backend" && go run ./tools/pluginpackager -source "$stage" -key "$key" -key-id "$publisher" -out "$output")
else
  # The official packager reuses a protected local publisher key outside this repo.
  (cd "$board_backend" && go run ./tools/pluginpackager -source "$stage" -out "$output")
fi
