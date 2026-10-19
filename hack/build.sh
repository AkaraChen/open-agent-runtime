#!/usr/bin/env bash
# Builds bin/oar for the host. On macOS the binary is built with cgo (for
# Virtualization.framework) and ad-hoc signed with the virtualization
# entitlement.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
mkdir -p bin
if [ "$(uname -s)" = Darwin ]; then
  CGO_ENABLED=1 go build -trimpath -o bin/oar ./cmd/oar
  codesign --force --sign - --entitlements hack/vz.entitlements bin/oar
else
  CGO_ENABLED=0 go build -trimpath -o bin/oar ./cmd/oar
fi
echo "built $ROOT/bin/oar"
