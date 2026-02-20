# Homebrew Formula for CodeWire
class Codewire < Formula
  desc "Persistent process server for AI coding agents"
  homepage "https://github.com/codespacesh/codewire"
  version "0.2.18"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-apple-darwin"
      sha256 "f11e1ed2d16c860a5d768695202bd49a1a197ace76e85f879389d22cf3694a86"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-apple-darwin"
      sha256 "279750d7113ae5837491639ee467a41194acfce5389211f719a6de43c4007a71"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-unknown-linux-gnu"
      sha256 "be137c7cd0a1c6c9613457da024545db439606f8debca9536b3efe19a31dfa39"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-unknown-linux-musl"
      sha256 "b7fc6ece7dce3823ac71d44cb9e978ae963bb69a6790cff1ef07d13581f5b412"
    end
  end

  def install
    # Determine the correct binary name based on platform
    if OS.mac?
      if Hardware::CPU.arm?
        binary_name = "cw-v#{version}-aarch64-apple-darwin"
      else
        binary_name = "cw-v#{version}-x86_64-apple-darwin"
      end
    else
      if Hardware::CPU.arm?
        binary_name = "cw-v#{version}-aarch64-unknown-linux-gnu"
      else
        binary_name = "cw-v#{version}-x86_64-unknown-linux-musl"
      end
    end

    bin.install binary_name => "cw"
  end

  test do
    # Test that the binary runs and shows help
    assert_match "Persistent process server", shell_output("#{bin}/cw --help")

    # Test version display
    system "#{bin}/cw", "--version"
  end

  def caveats
    <<~EOS
      CodeWire node will auto-start on first command.

      Quick start:
        cw launch -- claude -p "your prompt here"
        cw list
        cw attach 1

      For more information:
        https://github.com/codespacesh/codewire
    EOS
  end
end
