#!/usr/bin/env python3
"""Validate the repository's Codex Skill without third-party Python packages."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


NAME_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
LINK_RE = re.compile(r"\[[^]]+\]\(([^)#]+)(?:#[^)]+)?\)")


def fail(message: str) -> None:
    raise ValueError(message)


def parse_frontmatter(text: str) -> tuple[dict[str, str], str]:
    lines = text.splitlines()
    if not lines or lines[0] != "---":
        fail("SKILL.md must start with YAML frontmatter")
    try:
        end = lines.index("---", 1)
    except ValueError as exc:
        raise ValueError("SKILL.md frontmatter is not closed") from exc

    values: dict[str, str] = {}
    for number, line in enumerate(lines[1:end], start=2):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        match = re.fullmatch(r"([a-z_]+):\s*(.+)", line)
        if not match:
            fail(f"unsupported frontmatter syntax on line {number}")
        key, value = match.groups()
        values[key] = value.strip().strip('"').strip("'")
    return values, "\n".join(lines[end + 1 :])


def validate_openai_yaml(path: Path, skill_name: str) -> None:
    if not path.is_file():
        fail("agents/openai.yaml is required")
    text = path.read_text(encoding="utf-8")
    required = ("display_name", "short_description", "default_prompt")
    for key in required:
        match = re.search(rf"^\s{{2}}{key}:\s*(.+)$", text, re.MULTILINE)
        if not match:
            fail(f"agents/openai.yaml is missing interface.{key}")
        value = match.group(1).strip()
        if not (value.startswith('"') and value.endswith('"')):
            fail(f"agents/openai.yaml interface.{key} must be quoted")
    if f"${skill_name}" not in text:
        fail("agents/openai.yaml default_prompt must mention the skill by $name")
    short = re.search(r'^\s{2}short_description:\s*"([^"]+)"$', text, re.MULTILINE)
    if short and not 25 <= len(short.group(1)) <= 64:
        fail("agents/openai.yaml short_description must be 25-64 characters")


def validate(skill_dir: Path) -> None:
    skill_file = skill_dir / "SKILL.md"
    if not skill_file.is_file():
        fail("SKILL.md is required")
    if any(path.name.lower() == "readme.md" for path in skill_dir.iterdir()):
        fail("a Skill must not include its own README.md")

    text = skill_file.read_text(encoding="utf-8")
    metadata, body = parse_frontmatter(text)
    if set(metadata) != {"name", "description"}:
        fail("SKILL.md frontmatter must contain only name and description")
    name = metadata["name"]
    if not NAME_RE.fullmatch(name) or len(name) > 63:
        fail("skill name must be <=63 lowercase letters, digits, or hyphens")
    if skill_dir.name != name:
        fail("skill directory name must match frontmatter name")
    description = metadata["description"]
    if not description or "TODO" in description or len(description) > 1024:
        fail("skill description must be complete and <=1024 characters")
    if "TODO" in body:
        fail("SKILL.md still contains TODO text")
    if len(body.splitlines()) > 500:
        fail("SKILL.md body must stay below 500 lines")

    for target in LINK_RE.findall(body):
        linked = (skill_dir / target).resolve()
        try:
            linked.relative_to(skill_dir.resolve())
        except ValueError:
            fail(f"SKILL.md link escapes the skill directory: {target}")
        if not linked.is_file():
            fail(f"SKILL.md link does not exist: {target}")

    validate_openai_yaml(skill_dir / "agents" / "openai.yaml", name)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("skill_dir", type=Path)
    args = parser.parse_args()
    try:
        validate(args.skill_dir.resolve())
    except (OSError, UnicodeError, ValueError) as exc:
        print(f"skill validation failed: {exc}", file=sys.stderr)
        return 1
    print(f"skill validation passed: {args.skill_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
