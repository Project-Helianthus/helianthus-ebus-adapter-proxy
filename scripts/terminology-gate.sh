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

if command -v rg >/dev/null 2>&1; then
  search_prohibited_term() {
    local term="$1"
    rg -n -i -w \
      --hidden \
      --glob '!.git' \
      --glob '!scripts/terminology-gate.sh' \
      "${term}" .
  }
else
  search_prohibited_term() {
    local term="$1"
    grep -R -n -i -w -E \
      --binary-files=without-match \
      --exclude-dir='.git' \
      --exclude='terminology-gate.sh' \
      "${term}" .
  }
fi

for term in "${PROHIBITED_TERMS[@]}"; do
  if search_prohibited_term "${term}"; then
    echo "Terminology gate failed: found prohibited term '${term}'."
    exit 1
  fi
done

echo "Terminology gate passed."
