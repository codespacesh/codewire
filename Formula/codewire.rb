# Homebrew Formula for CodeWire
class Codewire < Formula
  desc "Persistent process server for AI coding agents"
  homepage "https://github.com/codespacesh/codewire"
  version "0.1.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-apple-darwin"
      sha256 "712f635fba60159e41fe0e211c147632397fd3ae3ced67f4c95b384d8774a960"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-apple-darwin"
      sha256 "5c41aac5ddf47add3d13a289d66f8882a5aa88fb78e94eba569256a6bb27207c"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-unknown-linux-gnu"
      sha256 "49ae7a7c51cab3ab703fc57a3143ac2ff933d454e2709dd387b495352c38b56a"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-unknown-linux-musl"
      sha256 "777309657af201d7cce09ebcb7af7b3ab7cabfac8dfdb3b93776373f79d03711"
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
      CodeWire daemon will auto-start on first command.

      Quick start:
        cw launch -- claude -p "your prompt here"
        cw list
        cw attach 1

      For more information:
        https://github.com/codespacesh/codewire
    EOS
  end
end
