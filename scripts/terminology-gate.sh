#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

PROHIBITED_TERMS=(
  "master"
  "slave"
  "whitelist"
  "blacklist"
)

for term in "${PROHIBITED_TERMS[@]}"; do
  if rg -n -i -w \
    --hidden \
    --glob '!.git' \
    --glob '!scripts/terminology-gate.sh' \
    "${term}" .; then
    echo "Terminology gate failed: found prohibited term '${term}'."
    exit 1
  fi
done

echo "Terminology gate passed."
