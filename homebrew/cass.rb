class Cass < Formula
  desc "Cassandra platform CLI — auth, MCP keys, cookies, codex/claude setup"
  homepage "https://github.com/Cassandras-Edge/cass"
  license "Proprietary"
  version "0.10.3" # bump-homebrew: VERSION

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-arm64"
      sha256 "beda67cd705e8f1a1fc88c2cede0cd53ef2be833b883eee86862560cef803c61" # bump-homebrew: SHA_DARWIN_ARM64

      def install
        bin.install "cass-darwin-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-amd64"
      sha256 "7f6a3364bfb6e2165b9d19416dbe6e1c2f088db7ba4e4d1077af807be19a432c" # bump-homebrew: SHA_DARWIN_AMD64

      def install
        bin.install "cass-darwin-amd64" => "cass"
      end
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-arm64"
      sha256 "ba32b2e1d3197eedc356381182cb62e09ce8d2a10450b59c3e61df5d18f30d3f" # bump-homebrew: SHA_LINUX_ARM64

      def install
        bin.install "cass-linux-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-amd64"
      sha256 "d7a378c3e6576a44418d3e9cc7c6a9291c84d4fea726a716d54f83afb296447a" # bump-homebrew: SHA_LINUX_AMD64

      def install
        bin.install "cass-linux-amd64" => "cass"
      end
    end
  end

  test do
    assert_match "Usage", shell_output("#{bin}/cass --help")
  end
end
