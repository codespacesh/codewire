# Homebrew Tap Setup Guide

This guide covers setting up a Homebrew tap for CodeWire and eventually submitting to Homebrew core.

## Option 1: Create Your Own Tap (Immediate)

### Step 1: Create the Tap Repository

Create a new repository at: `https://github.com/codespacesh/homebrew-tap`

The repository structure should be:
```
homebrew-tap/
├── Formula/
│   └── codewire.rb
└── README.md
```

### Step 2: Copy the Formula

Copy `Formula/codewire.rb` from this repository to `homebrew-tap/Formula/codewire.rb`

### Step 3: Update SHA256 Checksums

After each release, update the SHA256 checksums in the formula:

```bash
# Download your release
VERSION=v0.1.0
curl -L -O "https://github.com/codespacesh/codewire/releases/download/${VERSION}/SHA256SUMS"

# Get checksums for each platform
grep "aarch64-apple-darwin" SHA256SUMS
grep "x86_64-apple-darwin" SHA256SUMS
grep "aarch64-unknown-linux-gnu" SHA256SUMS
grep "x86_64-unknown-linux-musl" SHA256SUMS
```

Update these values in the formula file.

### Step 4: Test the Formula Locally

```bash
# Install from local file
brew install --build-from-source ./Formula/codewire.rb

# Test it works
cw --help

# Uninstall for clean testing
brew uninstall codewire

# Test from your tap
brew tap codespacesh/tap
brew install codewire
```

### Step 5: Commit and Push

```bash
cd homebrew-tap
git add Formula/codewire.rb
git commit -m "Add codewire formula v0.1.0"
git push origin main
```

### Step 6: Users Install Via Tap

Users can now install with:

```bash
brew tap codespacesh/tap
brew install codewire
```

## Option 2: Submit to Homebrew Core (Future)

Once CodeWire meets the criteria, you can submit to `homebrew-core` for `brew install codewire` (no tap needed).

### Requirements for Homebrew Core

From [Homebrew Acceptable Formulae](https://docs.brew.sh/Acceptable-Formulae):

1. **Popularity & Notability**
   - ✅ Should be useful to a broad audience
   - ✅ Should be actively maintained
   - ✅ Preferably 30+ GitHub stars (current: check repo)
   - ✅ Preferably 75+ watchers
   - ✅ Notable/noteworthy to general developers

2. **Technical Requirements**
   - ✅ Stable release version (no pre-releases)
   - ✅ Open source license (MIT ✓)
   - ✅ Builds cleanly without errors
   - ✅ Passes automated tests
   - ✅ No vendored dependencies (Rust is fine)

3. **Additional Criteria**
   - ✅ Not a duplicate of existing formula
   - ✅ Not a fork of another project
   - ✅ Not a web app or GUI-only tool
   - ✅ Not a library (CLI binary ✓)

### When to Submit

Wait until:
- [x] You have a stable v1.0 release (or close to it)
- [x] 30+ GitHub stars
- [x] Multiple releases with good stability
- [x] Active user base and positive feedback

### How to Submit to Homebrew Core

1. **Fork homebrew-core**

```bash
cd "$(brew --repository homebrew/core)"
git checkout master
git pull origin master
git checkout -b codewire
```

2. **Create the Formula**

```bash
# Homebrew provides a helper
brew create https://github.com/codespacesh/codewire/archive/v0.1.0.tar.gz

# Or manually
cp /path/to/your/codewire.rb Formula/codewire.rb
```

3. **Test Thoroughly**

```bash
brew install --build-from-source codewire
brew test codewire
brew audit --new-formula codewire
brew audit --strict codewire
```

4. **Submit Pull Request**

```bash
git add Formula/codewire.rb
git commit -m "codewire 0.1.0 (new formula)"
git push your-fork codewire
```

Open a PR at: https://github.com/Homebrew/homebrew-core/pulls

5. **Address Feedback**

Homebrew maintainers will review and may request changes:
- Formula style/syntax
- Test coverage
- Dependencies
- Platform support

## Automating Formula Updates

### GitHub Actions Automation

Create `.github/workflows/update-homebrew.yml` in your tap repo:

```yaml
name: Update Formula

on:
  repository_dispatch:
    types: [new-release]

jobs:
  update:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Update formula
        env:
          VERSION: ${{ github.event.client_payload.version }}
        run: |
          # Download checksums
          curl -L -O "https://github.com/codespacesh/codewire/releases/download/${VERSION}/SHA256SUMS"

          # Extract checksums
          ARM64_MAC=$(grep "aarch64-apple-darwin" SHA256SUMS | cut -d' ' -f1)
          X64_MAC=$(grep "x86_64-apple-darwin" SHA256SUMS | cut -d' ' -f1)
          ARM64_LINUX=$(grep "aarch64-unknown-linux-gnu" SHA256SUMS | cut -d' ' -f1)
          X64_LINUX=$(grep "x86_64-unknown-linux-musl" SHA256SUMS | cut -d' ' -f1)

          # Update formula
          sed -i "s/version \".*\"/version \"${VERSION#v}\"/" Formula/codewire.rb
          sed -i "s/PUT_ARM64_SHA256_HERE/${ARM64_MAC}/" Formula/codewire.rb
          sed -i "s/PUT_X86_64_SHA256_HERE/${X64_MAC}/" Formula/codewire.rb
          sed -i "s/PUT_LINUX_ARM64_SHA256_HERE/${ARM64_LINUX}/" Formula/codewire.rb
          sed -i "s/PUT_LINUX_X86_64_SHA256_HERE/${X64_LINUX}/" Formula/codewire.rb

      - name: Commit changes
        run: |
          git config user.name "GitHub Actions"
          git config user.email "actions@github.com"
          git add Formula/codewire.rb
          git commit -m "Update codewire to ${VERSION}"
          git push
```

Trigger from your main repo's release workflow:

```yaml
- name: Update Homebrew tap
  run: |
    curl -X POST \
      -H "Authorization: token ${{ secrets.TAP_REPO_TOKEN }}" \
      -H "Accept: application/vnd.github.v3+json" \
      https://api.github.com/repos/codespacesh/homebrew-tap/dispatches \
      -d "{\"event_type\":\"new-release\",\"client_payload\":{\"version\":\"${{ github.ref_name }}\"}}"
```

## Quick Reference

### For Each Release

**Manual Process:**
1. Tag and push release (GitHub Actions builds binaries)
2. Download `SHA256SUMS` from release
3. Update formula with new version and checksums
4. Test: `brew install --build-from-source ./Formula/codewire.rb`
5. Commit and push to tap repo

**Automated Process:**
1. Tag and push release
2. GitHub Actions automatically updates tap formula

### Testing Commands

```bash
# Audit formula
brew audit --strict codewire

# Test installation
brew install --build-from-source codewire

# Test binary
cw --help

# Test formula
brew test codewire

# Uninstall for clean testing
brew uninstall codewire
```

## Resources

- [Homebrew Formula Cookbook](https://docs.brew.sh/Formula-Cookbook)
- [Acceptable Formulae](https://docs.brew.sh/Acceptable-Formulae)
- [Node for Formula Authors](https://docs.brew.sh/Node-for-Formula-Authors)
- [Python for Formula Authors](https://docs.brew.sh/Python-for-Formula-Authors)
- [Homebrew Tap Creation](https://docs.brew.sh/How-to-Create-and-Maintain-a-Tap)
