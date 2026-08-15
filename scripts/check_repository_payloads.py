#!/usr/bin/env python3
"""Reject sensitive runtime data and redistributable client packages in Git."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path


FORBIDDEN_SUFFIXES = {
    ".apk",
    ".appimage",
    ".deb",
    ".exe",
    ".jks",
    ".keystore",
    ".msi",
    ".p12",
    ".pfx",
    ".rpm",
    ".sqlite",
    ".sqlite3",
}
FORBIDDEN_PARTS = {
    ".xwechat",
    "auth-challenges",
    "client-profile",
    "xwechat_files",
}
SECRET_NAMES = {
    ".env",
    "id_ed25519",
    "id_rsa",
}


def repository_files() -> list[Path]:
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        check=True,
        stdout=subprocess.PIPE,
    )
    return [Path(raw.decode("utf-8")) for raw in result.stdout.split(b"\0") if raw]


def reason(path: Path) -> str | None:
    lower_parts = {part.lower() for part in path.parts}
    suffix = path.suffix.lower()
    name = path.name.lower()
    if suffix in FORBIDDEN_SUFFIXES:
        return f"forbidden artifact suffix {suffix}"
    if lower_parts & FORBIDDEN_PARTS:
        return "runtime/account-state path"
    if name in SECRET_NAMES or name.endswith(".pem") or name.endswith(".key"):
        return "credential-like filename"
    if name.startswith("wechat") and suffix in {".bin", ".zip", ".tar", ".gz"}:
        return "possible packaged official client"
    if name.startswith(("wxwork", "wecom")) and suffix in {".bin", ".zip", ".tar", ".gz"}:
        return "possible packaged official client"
    return None


def main() -> int:
    violations = [(path, reason(path)) for path in repository_files() if reason(path)]
    if violations:
        for path, detail in violations:
            print(f"forbidden repository payload: {path}: {detail}", file=sys.stderr)
        return 1
    print("repository payload check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
