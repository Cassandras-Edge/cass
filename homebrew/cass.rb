class Cass < Formula
  desc "Cassandra platform CLI — auth, MCP keys, cookies, codex/claude setup"
  homepage "https://github.com/Cassandras-Edge/cass"
  license "Proprietary"
  version "0.11.0" # bump-homebrew: VERSION

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-arm64"
      sha256 "196aed87dc6c996811a27a977c4c0f47271db351f7cacd2e13277925cb3a2110" # bump-homebrew: SHA_DARWIN_ARM64

      def install
        bin.install "cass-darwin-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-amd64"
      sha256 "99f6a5dd6c685aeae8a22136f3294e06c9dba954c9d6891bfe478eccdff4d57a" # bump-homebrew: SHA_DARWIN_AMD64

      def install
        bin.install "cass-darwin-amd64" => "cass"
      end
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-arm64"
      sha256 "447b9be501e465ca1192fb6db85dda080d63a4f256d63862c205bd2b5d3d03a5" # bump-homebrew: SHA_LINUX_ARM64

      def install
        bin.install "cass-linux-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-amd64"
      sha256 "efca382eebbf80a5c04c3e087b32c91e1fe9d631b53f5d92aad95bbc4baccfe1" # bump-homebrew: SHA_LINUX_AMD64

      def install
        bin.install "cass-linux-amd64" => "cass"
      end
    end
  end

  test do
    assert_match "Usage", shell_output("#{bin}/cass --help")
  end
end
