# Homebrew Formula for CodeWire
class Codewire < Formula
  desc "Persistent process server for AI coding agents"
  homepage "https://github.com/codespacesh/codewire"
  version "0.2.32"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-apple-darwin"
      sha256 "46083d2b9e39c568d7895fbd455b2b0384e1606db4254a9eb89cb6db4a2a10a3"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-apple-darwin"
      sha256 "78b11c493bfdadb27c7abe3acf015e548d9af83d9f0e9a2b2de7111c74a88d81"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-aarch64-unknown-linux-gnu"
      sha256 "7007e331fa061016d252e98c0b0ec02c68d4abbfd620c0a52a66a4ae54ee28fd"
    else
      url "https://github.com/codespacesh/codewire/releases/download/v#{version}/cw-v#{version}-x86_64-unknown-linux-musl"
      sha256 "f19b5c47797cb1c3fd15b486d1d63e232f6cf432ebef4ab768826fb04fe2ee22"
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
