#!/usr/bin/env python3
from __future__ import annotations

import argparse
import concurrent.futures
import json
import statistics
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


def send(url: str, api_key: str, index: int) -> tuple[int, float]:
    payload = json.dumps(
        {
            "strategy": "sklearn",
            "text": f"The kitchen sink pipe is leaking and needs help this week request {index}",
        }
    ).encode()
    request = urllib.request.Request(
        url,
        data=payload,
        method="POST",
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
    )
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            response.read()
            status = response.status
    except urllib.error.HTTPError as error:
        error.read()
        status = error.code
    return status, (time.perf_counter() - started) * 1000


def percentile(values: list[float], percentile_value: float) -> float:
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, int(round((len(ordered) - 1) * percentile_value))))
    return ordered[index]


def assert_all_success(statuses: dict[str, int], expected_requests: int) -> None:
    successful = statuses.get("200", 0)
    if successful != expected_requests:
        raise RuntimeError(
            f"expected {expected_requests} successful responses, got {successful}: {statuses}"
        )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--api-key", required=True)
    parser.add_argument("--requests", type=int, default=100)
    parser.add_argument("--concurrency", type=int, default=10)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.requests <= 0 or args.concurrency <= 0:
        raise SystemExit("requests and concurrency must be positive")

    started = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        results = list(
            pool.map(lambda index: send(args.url, args.api_key, index), range(args.requests))
        )
    elapsed = time.perf_counter() - started
    statuses: dict[str, int] = {}
    latencies = []
    for status, latency in results:
        statuses[str(status)] = statuses.get(str(status), 0) + 1
        latencies.append(latency)
    report: dict[str, Any] = {
        "requests": args.requests,
        "concurrency": args.concurrency,
        "elapsed_seconds": elapsed,
        "requests_per_second": args.requests / elapsed,
        "status_counts": statuses,
        "latency_ms": {
            "min": min(latencies),
            "mean": statistics.fmean(latencies),
            "p50": percentile(latencies, 0.50),
            "p95": percentile(latencies, 0.95),
            "p99": percentile(latencies, 0.99),
            "max": max(latencies),
        },
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    print(json.dumps(report, indent=2, sort_keys=True))
    try:
        assert_all_success(statuses, args.requests)
    except RuntimeError as error:
        raise SystemExit(str(error)) from error


if __name__ == "__main__":
    main()
