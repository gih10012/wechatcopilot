#!/usr/bin/env python3
"""Validate a go-licenses CSV report against the repository SPDX allowlist."""

from __future__ import annotations

import argparse
import csv
import re
import sys
from pathlib import Path


ALIASES = {
    "BSD-2-Clause-FreeBSD": "BSD-2-Clause",
    "BSD-3-Clause-Clear": "BSD-3-Clause",
}


def read_allowed(path: Path) -> set[str]:
    return {
        line.strip()
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.strip() and not line.lstrip().startswith("#")
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("report", type=Path, help="CSV output from go-licenses report")
    parser.add_argument(
        "--allowlist", type=Path, default=Path("LICENSES/allowed.txt")
    )
    args = parser.parse_args()
    allowed = read_allowed(args.allowlist)
    failures: list[str] = []
    rows_seen = 0
    with args.report.open(newline="", encoding="utf-8") as handle:
        for row in csv.reader(handle):
            rows_seen += 1
            if len(row) < 3:
                failures.append(f"malformed report row: {row!r}")
                continue
            package, _url, detected = row[:3]
            licenses = [
                item
                for item in re.split(r"[\s,;()]+", detected)
                if item and item not in {"AND", "OR", "WITH"}
            ]
            normalized = {ALIASES.get(item, item) for item in licenses if item}
            if not normalized:
                failures.append(f"{package}: no license detected")
            elif not normalized <= allowed:
                failures.append(
                    f"{package}: disallowed license(s): {', '.join(sorted(normalized - allowed))}"
                )
    if rows_seen == 0:
        failures.append("license report is empty")
    if failures:
        print("dependency license check failed:", file=sys.stderr)
        for failure in failures:
            print(f"- {failure}", file=sys.stderr)
        return 1
    print("dependency license check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
