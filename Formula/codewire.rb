# Homebrew Formula for CodeWire
class Codewire < Formula
  desc "Persistent process server for AI coding agents"
  homepage "https://github.com/codespacesh/codewire"
  version "0.2.31"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-apple-darwin"
      sha256 "eb15657fb21f425ad0e117cdbfbbb16f541afda87328e4a8c3ad7ae87c343d44"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-apple-darwin"
      sha256 "13304cc64afbca8f6b34cac662b7b958c70fff7be17e1fd6cf8265d5b1df3428"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-unknown-linux-gnu"
      sha256 "2eec8554282d042b88cc9723e8be0fab04efbaa258e27058f39211d8fd154d06"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-unknown-linux-musl"
      sha256 "dd5d18c5394143362cf68480e2ded5c05788f8bf0a3b947e3f458f1bedd8f0a8"
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
    generate_completions_from_executable(bin/"cw", "completion")
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
