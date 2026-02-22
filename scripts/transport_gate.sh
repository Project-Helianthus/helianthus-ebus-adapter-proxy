#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

base_ref="${TRANSPORT_GATE_BASE_REF:-origin/main}"
if ! git rev-parse --verify "${base_ref}" >/dev/null 2>&1; then
  base_ref="main"
fi
if ! git rev-parse --verify "${base_ref}" >/dev/null 2>&1; then
  echo "transport gate: base ref not found, skipping."
  exit 0
fi

changed_files="$(git diff --name-only "${base_ref}...HEAD")"
if [[ -z "${changed_files}" ]]; then
  echo "transport gate: no changes against ${base_ref}."
  exit 0
fi

requires_gate=0
while IFS= read -r file; do
  [[ -z "${file}" ]] && continue
  if [[ "${file}" =~ ^(internal/adapterproxy/|internal/southbound/|internal/sourcepolicy/|internal/scheduler/|cmd/helianthus-ebus-adapter-proxy/) ]]; then
    requires_gate=1
    break
  fi
done <<< "${changed_files}"

if [[ "${requires_gate}" -eq 0 ]]; then
  echo "transport gate: not triggered."
  exit 0
fi

if [[ "${TRANSPORT_GATE_OWNER_OVERRIDE:-}" == "OVERRIDE_TRANSPORT_GATE_BY_OWNER" ]]; then
  if [[ -z "${TRANSPORT_GATE_OWNER_REASON:-}" ]]; then
    echo "transport gate override requires TRANSPORT_GATE_OWNER_REASON."
    exit 1
  fi
  echo "transport gate: owner override active (${TRANSPORT_GATE_OWNER_REASON})."
  exit 0
fi

report_path="${TRANSPORT_MATRIX_REPORT:-}"
if [[ -z "${report_path}" ]]; then
  echo "transport gate: TRANSPORT_MATRIX_REPORT is required for transport/protocol changes."
  exit 1
fi
if [[ ! -f "${report_path}" ]]; then
  echo "transport gate: report not found at ${report_path}."
  exit 1
fi

python3 - "${report_path}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as handle:
    payload = json.load(handle)

cases = payload.get("cases")
if not isinstance(cases, list):
    print("transport gate: invalid matrix report (missing cases list).")
    raise SystemExit(1)
if len(cases) != 88:
    print(f"transport gate: expected 88 cases, got {len(cases)}.")
    raise SystemExit(1)

def normalized_outcome(case):
    outcome = case.get("outcome")
    if isinstance(outcome, str) and outcome:
        return outcome
    status = case.get("status")
    if status == "passed":
        return "pass"
    if status == "planned":
        return "planned"
    return "fail"

unexpected = []
xfailed = 0
xpassed = 0
passed = 0
blocked = 0
blocked_invalid = []
for case in cases:
    value = normalized_outcome(case)
    case_id = case.get("case_id", "?")
    if value == "pass":
        passed += 1
    elif value == "xfail":
        xfailed += 1
    elif value == "xpass":
        xpassed += 1
    elif value == "blocked-infra":
        reason = str(case.get("infra_reason", "")).strip()
        if reason != "adapter_no_signal":
            blocked_invalid.append((case_id, reason))
            continue
        blocked += 1
    else:
        unexpected.append(case_id)

if blocked_invalid:
    preview = ",".join(f"{case_id}:{reason or 'missing'}" for case_id, reason in blocked_invalid[:10])
    print(f"transport gate: matrix has blocked-infra with unsupported reason ({len(blocked_invalid)}). sample={preview}")
    raise SystemExit(1)

if unexpected:
    preview = ",".join(unexpected[:10])
    print(f"transport gate: matrix has unexpected failures/planned ({len(unexpected)}). sample={preview}")
    raise SystemExit(1)

msg = f"transport gate: PASS (pass={passed}, xfail={xfailed}, xpass={xpassed}, blocked={blocked}, total={len(cases)})."
if xpassed:
    msg += " review expected-failure list (xpass present)."
print(msg)
PY
