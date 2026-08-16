#!/usr/bin/env python3
"""Allow only project-owned artifact names in a release staging directory."""

from __future__ import annotations

import argparse
import re
import sys
import tarfile
from pathlib import Path


GO_ARCHIVE = re.compile(r"wechatcopilot-linux-(amd64|arm64)\.tar\.gz$")
REQUIRED_RELEASE_FILES = {
    "checksums.txt",
    "wechatcopilot-linux-amd64.tar.gz",
    "wechatcopilot-linux-arm64.tar.gz",
    "wechatcopilot-companion.apk",
    "wechatcopilot.spdx.json",
}
REQUIRED_ARCHIVE_FILES = {
    "LICENSE",
    "LICENSES/Apache-2.0.txt",
    "LICENSES/BSD-3-Clause-modernc-sqlite.txt",
    "LICENSES/MIT-dependencies.txt",
    "LICENSES/README.md",
    "LICENSES/allowed.txt",
    "README.md",
    "THIRD_PARTY_NOTICES.md",
    "deploy/wechat/Dockerfile",
    "deploy/wechat/README.md",
    "deploy/wechat/control.sh",
    "deploy/wechat/entrypoint.sh",
    "deploy/wechat/screenshot.sh",
    "deploy/wechat/session-entrypoint.sh",
    "deploy/wechat/test_ui_driver.py",
    "deploy/wechat/ui_driver.py",
    "docs/agent-guide.md",
    "docs/architecture.md",
    "docs/install.md",
    "docs/mcp.md",
    "docs/releasing.md",
    "docs/security.md",
    "scripts/provision_state_volume.sh",
    "skills/wechatcopilot/SKILL.md",
    "skills/wechatcopilot/agents/openai.yaml",
    "skills/wechatcopilot/references/accounts.md",
    "skills/wechatcopilot/references/cli.md",
    "skills/wechatcopilot/references/mcp.md",
    "skills/wechatcopilot/references/safety.md",
    "wechatcopilot",
}
REQUIRED_ARCHIVE_DIRECTORIES = {
    "LICENSES",
    "LICENSES/go",
    "deploy",
    "deploy/wechat",
    "docs",
    "scripts",
    "skills",
    "skills/wechatcopilot",
    "skills/wechatcopilot/agents",
    "skills/wechatcopilot/references",
}
REQUIRED_ARCHIVE_MEMBERS = REQUIRED_ARCHIVE_FILES | REQUIRED_ARCHIVE_DIRECTORIES


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    errors: list[str] = []
    entries = sorted(args.directory.iterdir())
    files = []
    for path in entries:
        if path.is_symlink() or not path.is_file():
            errors.append(f"release artifact must be a regular file: {path.name}")
            continue
        files.append(path)
    release_names = {path.name for path in files}
    missing_release_files = REQUIRED_RELEASE_FILES - release_names
    unexpected_release_files = release_names - REQUIRED_RELEASE_FILES
    if missing_release_files:
        errors.append(f"missing release artifacts: {sorted(missing_release_files)}")
    if unexpected_release_files:
        errors.append(f"unexpected release artifacts: {sorted(unexpected_release_files)}")
    for path in files:
        if GO_ARCHIVE.fullmatch(path.name):
            try:
                with tarfile.open(path, "r:gz") as archive:
                    members = archive.getmembers()
                    names = {member.name for member in members}
                    if len(names) != len(members):
                        errors.append(f"{path.name}: duplicate archive member names are forbidden")
                    if any(not (member.isfile() or member.isdir()) for member in members):
                        errors.append(f"{path.name}: links and special archive members are forbidden")
                    if any(
                        member.startswith("/") or ".." in Path(member).parts
                        for member in names
                    ):
                        errors.append(f"{path.name}: unsafe archive member path")
                    for member in members:
                        if member.name in REQUIRED_ARCHIVE_FILES and not member.isfile():
                            errors.append(f"{path.name}: {member.name} must be a regular file")
                        if member.name in REQUIRED_ARCHIVE_DIRECTORIES and not member.isdir():
                            errors.append(f"{path.name}: {member.name} must be a directory")
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
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print("release asset allowlist check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
