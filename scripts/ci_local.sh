#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

echo "==> gofmt"
unformatted="$(git ls-files '*.go' | xargs -n 50 gofmt -l || true)"
if [ -n "${unformatted}" ]; then
  echo "gofmt required for:"
  echo "${unformatted}"
  exit 1
fi

echo "==> go test"
GOWORK=off go test ./...

echo "==> go vet"
GOWORK=off go vet ./...

echo "==> terminology gate"
./scripts/terminology-gate.sh

echo "==> transport gate"
./scripts/transport_gate.sh

echo "==> runbook docs gate"
./scripts/runbook-gate.sh
