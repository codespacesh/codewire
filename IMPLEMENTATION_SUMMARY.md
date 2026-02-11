# Auto-Attach Implementation + LLM-Friendly Development

## Overview

This implementation adds two major improvements to codewire:

1. **Auto-attach feature** - `cw attach` now works without a session ID
2. **LLM-friendly development** - Comprehensive Claude Code skill + automated testing

## 1. Auto-Attach Feature

### Changes Made

**CLI (src/main.rs:67-71)**
```rust
// Made session ID optional
Attach {
    /// Session ID (omit to auto-select oldest unattached session)
    id: Option<u32>,
}
```

**Client (src/client.rs:97-142)**
- Updated `attach()` function to accept `Option<u32>`
- Auto-selection logic:
  1. Lists all sessions when ID is `None`
  2. Filters for `status == "running" && !attached`
  3. Sorts by `created_at` (oldest first)
  4. Selects first candidate
- Shows "(auto-selected)" in confirmation message
- Helpful error when no unattached sessions available

**Testing (tests/integration.rs)**
Added 5 comprehensive integration tests:
- ✅ `test_auto_attach_single_session` - Auto-select when only one session
- ✅ `test_auto_attach_oldest_session` - Select oldest among multiple
- ✅ `test_auto_attach_skips_attached` - Skip already attached sessions
- ✅ `test_auto_attach_skips_completed` - Skip completed sessions
- ✅ `test_auto_attach_no_candidates` - Error when none available
- ✅ `test_explicit_attach_still_works` - Explicit IDs still work

### Usage

```bash
# Auto-attach to oldest unattached session
cw attach

# Explicit ID still works
cw attach 3

# Helpful error when no sessions
cw attach
# Error: No unattached running sessions available.
#
# Use 'cw list' to see all sessions or 'cw launch' to start a new one
```

### Verification

**All tests pass:**
- 24 integration tests
- 4 unit tests
- Manual CLI test script

## 2. LLM-Friendly Development

### Files Created

**`.claude/skills/codewire-dev.md`**
Comprehensive development skill for LLMs including:
- Project structure overview
- Step-by-step command implementation guide
- Testing patterns and conventions
- **Coder CLI integration examples** (as requested)
- Common pitfalls
- Quick reference commands

**`.claude/skills/install.sh`**
One-line install script:
```bash
curl -fsSL https://raw.githubusercontent.com/sonica/codewire/main/.claude/skills/install.sh | bash
```

**`tests/manual_test.sh`**
Automated CLI integration test:
```bash
./tests/manual_test.sh ./target/release/cw
```

**`Makefile`**
Convenience commands:
```bash
make test-all      # Run all tests
make test-manual   # Run CLI integration test
make build         # Build release
make install       # Install to /usr/local/bin
make clean         # Clean everything
```

### Coder CLI Integration

The skill includes extensive Coder integration examples:

**Basic workflow:**
```bash
# SSH into workspace
coder ssh myworkspace

# Launch persistent sessions
cw launch -- cargo build --release
cw launch -- npm run dev

# Detach (processes keep running)
exit

# Monitor from local machine
coder ssh myworkspace -- cw watch 1
```

**Remote monitoring:**
```bash
# Watch remote session without SSH
coder ssh myworkspace -- cw watch 1

# Follow logs
coder ssh myworkspace -- cw logs 1 --follow

# Check status
coder ssh myworkspace -- cw list --json | jq
```

**Development patterns:**
- Background build server
- Multiple parallel workstreams
- Long-running training jobs
- Jupyter in background
- CI/CD integration

See `.claude/skills/codewire-dev.md` for complete examples.

### README Updates

Added "LLM-Friendly Development" section covering:
- Quick start for LLM contributors
- Key development principles
- Coder integration overview
- Development workflow
- Quick reference commands

## Testing & Verification

### Automated Tests

**Integration tests (cargo test):**
```bash
$ cargo test
running 24 tests
test test_auto_attach_single_session ... ok
test test_auto_attach_oldest_session ... ok
test test_auto_attach_skips_attached ... ok
test test_auto_attach_skips_completed ... ok
test test_auto_attach_no_candidates ... ok
test test_explicit_attach_still_works ... ok
[... 18 more tests ...]

test result: ok. 24 passed; 0 failed
```

**Manual CLI test:**
```bash
$ make test-manual
cargo build --release
./tests/manual_test.sh ./target/release/cw
=== Testing Auto-Attach Feature ===
✓ Help text correct
✓ Error message correct when no sessions available
=== All manual tests passed! ===
```

### Build Verification

```bash
$ cargo build --release
   Finished `release` profile [optimized] target(s) in 4.80s

$ cargo clippy --all-targets --all-features
   Finished in 0.00s (no warnings)
```

## For LLMs: How to Use This

### Installing the Skill

```bash
# From anywhere
curl -fsSL https://raw.githubusercontent.com/sonica/codewire/main/.claude/skills/install.sh | bash

# Or from repo root
./.claude/skills/install.sh
```

### Using the Skill

In Claude Code, simply mention:
```
Use the codewire-dev skill to implement [feature]
```

Or explicitly invoke:
```
/skill codewire-dev
```

### Making Changes

The skill guides you through:
1. Running existing tests
2. Understanding the architecture
3. Implementing new features
4. Adding integration tests
5. Manual verification
6. Common pitfalls to avoid

### Key Conventions

**Test naming:** Keep directory names ≤12 chars
```rust
temp_dir("auto-skip-done")  // ✅ Good
temp_dir("auto-attach-skip-completed")  // ❌ Too long
```

**Always add tests:** Every feature needs an integration test
```rust
#[tokio::test]
async fn test_new_feature() {
    let dir = temp_dir("new-feat");
    let sock = start_test_daemon(&dir).await;
    // Test implementation
}
```

**Run full suite:** Before committing
```bash
make test-all
```

## Summary

✅ **Auto-attach feature implemented and tested**
- Optional session ID in CLI
- Smart selection logic (oldest unattached)
- Comprehensive error messages
- 5 new integration tests
- Backward compatible (explicit IDs still work)

✅ **LLM-friendly development infrastructure**
- Comprehensive Claude Code skill
- Extensive Coder CLI examples (as requested)
- Automated test scripts
- Makefile for convenience
- Updated README with LLM section
- Easy install with shortcode

✅ **All tests passing**
- 24 integration tests
- 4 unit tests
- Manual CLI test script
- Clean builds with no warnings

## Files Changed

- `src/main.rs` - Made session ID optional
- `src/client.rs` - Auto-selection logic
- `tests/integration.rs` - Added 5 new tests
- `README.md` - Added LLM-friendly section

## Files Created

- `.claude/skills/codewire-dev.md` - Comprehensive development skill
- `.claude/skills/install.sh` - One-line skill installer
- `tests/manual_test.sh` - Automated CLI test
- `Makefile` - Convenience commands
- `IMPLEMENTATION_SUMMARY.md` - This file

## Next Steps

For LLMs working on codewire:
1. Install the skill: `./.claude/skills/install.sh`
2. Reference it when implementing features
3. Follow the test-driven workflow
4. Use `make test-all` before committing
5. Check the skill for Coder integration patterns

For users:
1. Run `cw attach` without session ID
2. Enjoy automatic session selection
3. Report issues on GitHub
