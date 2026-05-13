class Cass < Formula
  desc "Cassandra platform CLI — auth, MCP keys, cookies, codex/claude setup"
  homepage "https://github.com/Cassandras-Edge/cass"
  license "Proprietary"
  version "0.9.0" # bump-homebrew: VERSION

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-arm64"
      sha256 "801d97d33ff60ed5d06b675b7548780bd3a1b4ec939b697f5c8b3ad8320e07cc" # bump-homebrew: SHA_DARWIN_ARM64

      def install
        bin.install "cass-darwin-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-amd64"
      sha256 "ef1a4aee51e768cef44d7007e92d02ba22b6aee8cf4ccce0b910f1d54c5bda50" # bump-homebrew: SHA_DARWIN_AMD64

      def install
        bin.install "cass-darwin-amd64" => "cass"
      end
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-arm64"
      sha256 "d66183056387dec22b67b3dfa1d0cb9e7abf40045d7c2f42b953cd4ff830a723" # bump-homebrew: SHA_LINUX_ARM64

      def install
        bin.install "cass-linux-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-amd64"
      sha256 "77d07fe2ec39c6b8146f081cf3993fce5afa6d384df367ddf831c4e4fe319b04" # bump-homebrew: SHA_LINUX_AMD64

      def install
        bin.install "cass-linux-amd64" => "cass"
      end
    end
  end

  test do
    assert_match "Usage", shell_output("#{bin}/cass --help")
  end
end
