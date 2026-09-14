#!/usr/bin/env python3
"""Query every committed dashboard panel against a live Prometheus.

A dashboard is a file full of strings that are never compiled and never
imported. A metric renamed in Go breaks a panel and nothing fails -- until
someone opens Grafana during an incident and finds an empty graph.

This closes that gap from the other side: the package test checks that every
panel names a metric something exports, and this checks that the query
actually returns a series against a running system.

    make check-dashboards           # after exercising the stack

Panels listed under "expected to be empty" describe conditions that should not
be happening. A dead-letter counter with no data is the system working.
"""

from __future__ import annotations

import glob
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

PROMETHEUS = os.getenv("PROMETHEUS_URL", "http://localhost:9090")

# Panels that are empty when nothing is wrong. Listed by title so that a panel
# renamed without thought loses its exemption and has to be reconsidered.
EXPECTED_EMPTY = {
    "Cache errors": "Redis is reachable.",
    "Dead letters": "Nothing has been dead-lettered, which is the goal.",
    "Produce errors": "No publish has failed.",
    "Dropped messages": "No WebSocket client has fallen behind.",
    "Drop rate by type": "No WebSocket client has fallen behind.",
    "Inference errors": "No inference call has failed.",
    "Resident memory": "The process collector reads /proc; empty off Linux.",
}


def query(expr: str) -> tuple[bool, str]:
    url = f"{PROMETHEUS}/api/v1/query?" + urllib.parse.urlencode({"query": expr})
    try:
        with urllib.request.urlopen(url, timeout=10) as response:
            payload = json.load(response)
    except urllib.error.HTTPError as exc:
        # A 400 is a PromQL error, which is the most valuable thing this
        # script finds: an expression that parses in a reviewer's head and
        # not in Prometheus.
        detail = json.load(exc).get("error", "") if exc.headers.get(
            "Content-Type", "").startswith("application/json") else exc.reason
        return False, f"invalid query: {detail}"
    except urllib.error.URLError as exc:
        return False, f"prometheus unreachable: {exc}"

    if payload.get("status") != "success":
        return False, f"query error: {payload.get('error')}"
    if not payload["data"]["result"]:
        return False, "no data"
    return True, ""


def main() -> int:
    dashboards = sorted(glob.glob("deploy/grafana/dashboards/*.json"))
    if not dashboards:
        print("no dashboards found; run from the repository root", file=sys.stderr)
        return 1

    total = 0
    unexpected: list[tuple[str, str, str]] = []
    expected: list[tuple[str, str]] = []

    for path in dashboards:
        name = os.path.basename(path)
        with open(path) as fh:
            dashboard = json.load(fh)

        for panel in dashboard["panels"]:
            title = panel["title"]
            for target in panel["targets"]:
                total += 1
                ok, why = query(target["expr"])
                if ok:
                    continue
                if title in EXPECTED_EMPTY and why == "no data":
                    expected.append((name, title))
                else:
                    unexpected.append((name, title, why))

    print(f"{total} queries across {len(dashboards)} dashboards")

    if expected:
        seen = set()
        print("\nempty, and expected to be:")
        for name, title in expected:
            if title in seen:
                continue
            seen.add(title)
            print(f"  {title:32} {EXPECTED_EMPTY[title]}")

    if unexpected:
        print(f"\n{len(unexpected)} panel(s) returned nothing unexpectedly:")
        for name, title, why in unexpected:
            print(f"  {name:22} {title:32} {why}")
        return 1

    print("\nevery other panel returned data")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
