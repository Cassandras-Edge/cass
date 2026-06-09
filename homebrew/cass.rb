class Cass < Formula
  desc "Cassandra platform CLI — auth, MCP keys, cookies, codex/claude setup"
  homepage "https://github.com/Cassandras-Edge/cass"
  license "Proprietary"
  version "0.10.1" # bump-homebrew: VERSION

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-arm64"
      sha256 "72aaaa2b3df315ddd3e0c3ad65560ec53f778bba51b086a3514d214f1de24cf1" # bump-homebrew: SHA_DARWIN_ARM64

      def install
        bin.install "cass-darwin-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-amd64"
      sha256 "71f47f83c1a1b4d65a8eaf3766c4cf70c6ba7dfb926d97ca88e8f162e7757e77" # bump-homebrew: SHA_DARWIN_AMD64

      def install
        bin.install "cass-darwin-amd64" => "cass"
      end
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-arm64"
      sha256 "ce8318589de2a12386e8fb4cce2ddcf6c3418770156d9b2aef9114daa6f45bb0" # bump-homebrew: SHA_LINUX_ARM64

      def install
        bin.install "cass-linux-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-amd64"
      sha256 "14d35af5da89856767c532f6a4d239816231f2584d4d1f9c9965b5e619e4792e" # bump-homebrew: SHA_LINUX_AMD64

      def install
        bin.install "cass-linux-amd64" => "cass"
      end
    end
  end

  test do
    assert_match "Usage", shell_output("#{bin}/cass --help")
  end
end
