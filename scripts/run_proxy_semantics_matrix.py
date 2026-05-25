#!/usr/bin/env python3
"""Run the PX01..PX12 proxy wire-semantics matrix and write a gate report."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import pathlib
import subprocess
import sys


CASES = [
    {
        "case_id": "PX01",
        "focus": "stale STARTED absorb succeeds when matching result arrives in absorb window",
        "tests": ["TestDeliverPendingStartENHStartedMismatchAbsorbedWhenMatchingArrivesBounded"],
    },
    {
        "case_id": "PX02",
        "focus": "stale STARTED absorb expires via bounded fail path",
        "tests": ["TestDeliverPendingStartENHStartedMismatchExpiresBounded"],
    },
    {
        "case_id": "PX03",
        "focus": "SYN while waiting for command ACK reopens arbitration immediately",
        "tests": ["TestNoteBusWireSymbolReleasesOnSynWhileWaitingCommandAck"],
    },
    {
        "case_id": "PX04",
        "focus": "SYN while waiting for target response bytes reopens arbitration immediately",
        "tests": ["TestNoteBusWireSymbolReleasesOnSynWhileWaitingResponseBytes"],
    },
    {
        "case_id": "PX05",
        "focus": "same-boundary competition is resolved by FIFO registration order",
        "tests": ["TestHandleStartArbitrationSameBoundaryUsesFIFOAcrossInitiatorPriorities"],
    },
    {
        "case_id": "PX06",
        "focus": "queued higher initiator keeps its FIFO turn when a lower initiator arrives",
        "tests": ["TestXR_Arbitration_Fairness_NoStarvation"],
    },
    {
        "case_id": "PX07",
        "focus": "requeue-after-timeout receives a new FIFO position",
        "tests": ["TestHandleStartArbitrationRequeueAfterTimeoutKeepsFIFOAheadOfLowerInitiator"],
    },
    {
        "case_id": "PX08",
        "focus": "equal-initiator FIFO ordering is preserved",
        "tests": ["TestHandleStartArbitrationEqualInitiatorKeepsFIFO"],
    },
    {
        "case_id": "PX09",
        "focus": "local target observes request only from echoed RECEIVED path",
        "tests": ["TestTargetResponderWindowOpensFromEchoedRequestAndAllowsResponderSend"],
    },
    {
        "case_id": "PX10",
        "focus": "local emulated target response inside responder window remains coherent",
        "tests": ["TestTargetResponderWindowOpensFromEchoedRequestAndAllowsResponderSend"],
    },
    {
        "case_id": "PX11",
        "focus": "late responder bytes are rejected and counted",
        "tests": ["TestTargetResponderWindowRejectsLateResponderBytesAndCounts"],
    },
    {
        "case_id": "PX12",
        "focus": "non-owner/non-responder sends are rejected during active transaction",
        "tests": ["TestHandleSendRejectsNonOwnerNonResponderDuringActiveTransaction"],
    },
]


def repo_root() -> pathlib.Path:
    return pathlib.Path(__file__).resolve().parents[1]


def timestamp() -> str:
    return dt.datetime.now(dt.UTC).strftime("%Y%m%dT%H%M%SZ")


def run_case(root: pathlib.Path, case: dict[str, object], out_dir: pathlib.Path) -> dict[str, object]:
    tests = list(case["tests"])
    test_expr = "^(" + "|".join(tests) + ")$"
    command = [
        "go",
        "test",
        "./internal/adapterproxy",
        "-run",
        test_expr,
        "-count=1",
        "-v",
    ]
    env = os.environ.copy()
    env.setdefault("GOWORK", "off")
    result = subprocess.run(command, cwd=root, env=env, text=True, capture_output=True, check=False)

    log_name = f"{case['case_id']}.log"
    log_path = out_dir / log_name
    log_path.write_text(result.stdout + result.stderr, encoding="utf-8")

    outcome = "pass" if result.returncode == 0 else "fail"
    return {
        "case_id": case["case_id"],
        "focus": case["focus"],
        "outcome": outcome,
        "tests": tests,
        "log": log_name,
        "exit_code": result.returncode,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output-dir",
        default=None,
        help="directory for index.json and per-case logs (default: artifacts/proxy-semantics/<timestamp>)",
    )
    args = parser.parse_args()

    root = repo_root()
    out_dir = pathlib.Path(args.output_dir) if args.output_dir else root / "artifacts" / "proxy-semantics" / timestamp()
    if not out_dir.is_absolute():
        out_dir = root / out_dir
    out_dir.mkdir(parents=True, exist_ok=True)

    cases = [run_case(root, case, out_dir) for case in CASES]
    generated_at = dt.datetime.now(dt.UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    report = {
        "generated_at": generated_at,
        "suite": "proxy-wire-semantics",
        "case_set": "PX01..PX12",
        "repo": "Project-Helianthus/helianthus-ebus-adapter-proxy",
        "cases": cases,
    }

    report_path = out_dir / "index.json"
    report_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")

    passed = sum(1 for case in cases if case["outcome"] == "pass")
    failed = len(cases) - passed
    print(f"proxy semantics matrix complete: total={len(cases)} pass={passed} fail={failed}")
    print(report_path)
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
