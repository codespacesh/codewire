# Homebrew Tap for CodeWire

Official Homebrew tap for [CodeWire](https://github.com/codespacesh/codewire) - a persistent process server for AI coding agents.

## Installation

```bash
brew tap codespacesh/tap
brew install codewire
```

## Usage

```bash
# Launch a new session
cw launch -- claude -p "your prompt here"

# List sessions
cw list

# Attach to a session
cw attach 1

# View logs
cw logs 1
```

For full documentation, see: https://github.com/codespacesh/codewire

## Updating

```bash
brew update
brew upgrade codewire
```

## Uninstall

```bash
brew uninstall codewire
brew untap codespacesh/tap
```

## Formula

The formula is maintained at [Formula/codewire.rb](Formula/codewire.rb).

## Issues

Report issues at: https://github.com/codespacesh/codewire/issues
