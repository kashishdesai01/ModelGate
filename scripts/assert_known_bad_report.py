from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

EXPECTED_PREFIX = "critical slice noise=typo_emergency emergency_recall regressed by "


def assert_slice_only_rejection(envelope: dict[str, Any]) -> None:
    report = envelope.get("report")
    if not isinstance(report, dict) or report.get("passed") is not False:
        raise RuntimeError("known-bad report must be rejected")
    reasons = report.get("gate_reasons")
    if not isinstance(reasons, list) or len(reasons) != 1:
        raise RuntimeError(
            f"known-bad rejection must come only from the critical slice gate: {reasons!r}"
        )
    reason = reasons[0]
    if not isinstance(reason, str) or not reason.startswith(EXPECTED_PREFIX):
        raise RuntimeError(f"unexpected known-bad gate reason: {reason!r}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("report", type=Path)
    args = parser.parse_args()
    envelope = json.loads(args.report.read_text(encoding="utf-8"))
    assert_slice_only_rejection(envelope)
    print("known-bad candidate rejected only by the critical typo-emergency slice gate")


if __name__ == "__main__":
    main()
