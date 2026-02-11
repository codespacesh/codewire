#!/usr/bin/env bash
set -e

SKILL_NAME="codewire-dev"
SKILL_DIR="$HOME/.claude/skills"
SKILL_FILE="$SKILL_DIR/$SKILL_NAME.md"

echo "Installing Claude Code skill: $SKILL_NAME"

# Create skills directory if it doesn't exist
mkdir -p "$SKILL_DIR"

# Check if we're in the codewire repo
if [ -f ".claude/skills/codewire-dev.md" ]; then
    echo "✓ Found local skill file"
    cp ".claude/skills/codewire-dev.md" "$SKILL_FILE"
    echo "✓ Installed from local repository"
else
    # Download from GitHub
    echo "Downloading from GitHub..."
    curl -fsSL "https://raw.githubusercontent.com/sonica/codewire/main/.claude/skills/codewire-dev.md" -o "$SKILL_FILE"
    echo "✓ Downloaded and installed"
fi

echo
echo "Skill installed to: $SKILL_FILE"
echo
echo "To use in Claude Code:"
echo "  /skill $SKILL_NAME"
echo
echo "Or just mention 'codewire development' in your conversation"
