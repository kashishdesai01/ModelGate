from __future__ import annotations

import unittest

from scripts.assert_known_bad_report import assert_slice_only_rejection
from scripts.load_test import assert_all_success


class LoadTestExitPolicyTests(unittest.TestCase):
    def test_rejects_partial_success(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "100 successful responses"):
            assert_all_success({"200": 1, "503": 99}, 100)

    def test_accepts_all_success(self) -> None:
        assert_all_success({"200": 100}, 100)


class KnownBadReportPolicyTests(unittest.TestCase):
    def test_accepts_slice_only_rejection(self) -> None:
        assert_slice_only_rejection(
            {
                "report": {
                    "passed": False,
                    "gate_reasons": [
                        "critical slice noise=typo_emergency emergency_recall regressed by 0.0556 (support=36)"
                    ],
                }
            }
        )

    def test_rejects_aggregate_gate_failure(self) -> None:
        with self.assertRaises(RuntimeError):
            assert_slice_only_rejection(
                {
                    "report": {
                        "passed": False,
                        "gate_reasons": [
                            "critical slice noise=typo_emergency emergency_recall regressed by 0.0556 (support=36)",
                            "emergency recall regressed by 0.1000",
                        ],
                    }
                }
            )


if __name__ == "__main__":
    unittest.main()
