#!/usr/bin/env python3
"""Allow only project-owned artifact names in a release staging directory."""

from __future__ import annotations

import argparse
import re
import sys
import tarfile
from pathlib import Path


GO_ARCHIVE = re.compile(r"wechatcopilot-linux-(amd64|arm64)\.tar\.gz$")
SAFE_OTHER = {
    "checksums.txt",
    "wechatcopilot-companion.apk",
    "wechatcopilot.spdx.json",
}
REQUIRED_ARCHIVE_MEMBERS = {
    "LICENSE",
    "LICENSES",
    "LICENSES/Apache-2.0.txt",
    "LICENSES/BSD-3-Clause-modernc-sqlite.txt",
    "LICENSES/MIT-dependencies.txt",
    "LICENSES/README.md",
    "LICENSES/allowed.txt",
    "THIRD_PARTY_NOTICES.md",
    "skills",
    "skills/wechatcopilot",
    "skills/wechatcopilot/SKILL.md",
    "skills/wechatcopilot/agents",
    "skills/wechatcopilot/agents/openai.yaml",
    "skills/wechatcopilot/references",
    "skills/wechatcopilot/references/accounts.md",
    "skills/wechatcopilot/references/cli.md",
    "skills/wechatcopilot/references/mcp.md",
    "skills/wechatcopilot/references/safety.md",
    "wechatcopilot",
}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    errors: list[str] = []
    files = sorted(path for path in args.directory.iterdir() if path.is_file())
    for path in files:
        if GO_ARCHIVE.fullmatch(path.name):
            try:
                with tarfile.open(path, "r:gz") as archive:
                    members = archive.getmembers()
                    names = {member.name for member in members}
                    if any(not (member.isfile() or member.isdir()) for member in members):
                        errors.append(f"{path.name}: links and special archive members are forbidden")
                    if any(
                        member.startswith("/") or ".." in Path(member).parts
                        for member in names
                    ):
                        errors.append(f"{path.name}: unsafe archive member path")
                missing = REQUIRED_ARCHIVE_MEMBERS - names
                unexpected = {
                    name
                    for name in names
                    if name not in REQUIRED_ARCHIVE_MEMBERS
                    and name != "LICENSES/go"
                    and not name.startswith("LICENSES/go/")
                }
                if missing:
                    errors.append(f"{path.name}: missing archive members {sorted(missing)}")
                if unexpected:
                    errors.append(f"{path.name}: unexpected archive members {sorted(unexpected)}")
            except (OSError, tarfile.TarError) as exc:
                errors.append(f"{path.name}: invalid tar archive: {exc}")
        elif path.name not in SAFE_OTHER:
            errors.append(f"unexpected release artifact: {path.name}")
    if not any(GO_ARCHIVE.fullmatch(path.name) for path in files):
        errors.append("no Go release archive found")
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print("release asset allowlist check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
