#!/usr/bin/env bash
set -euo pipefail

SKILL_DIR="$HOME/.claude/skills"
REPO_URL="https://raw.githubusercontent.com/codespacesh/codewire/main/.claude/skills"

mkdir -p "$SKILL_DIR"

echo "Installing codewire skills..."

for skill in codewire.md codewire-dev.md; do
  curl -fsSL "$REPO_URL/$skill" -o "$SKILL_DIR/$skill"
  echo "  installed $skill"
done

echo
echo "Done. Skills available in Claude Code:"
echo "  codewire      — use codewire to manage persistent sessions"
echo "  codewire-dev  — develop on the codewire codebase"
