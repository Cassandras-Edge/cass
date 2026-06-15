class Cass < Formula
  desc "Cassandra platform CLI — auth, MCP keys, cookies, codex/claude setup"
  homepage "https://github.com/Cassandras-Edge/cass"
  license "Proprietary"
  version "0.10.4" # bump-homebrew: VERSION

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-arm64"
      sha256 "baf8d186e6b639baa214e9e001d04ffec0fcb6411d7f0c600c5faa83dd8f5501" # bump-homebrew: SHA_DARWIN_ARM64

      def install
        bin.install "cass-darwin-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-darwin-amd64"
      sha256 "8bab647c8fc828203599595a95a19416cab309bab50a455ea518181de6bc1c9c" # bump-homebrew: SHA_DARWIN_AMD64

      def install
        bin.install "cass-darwin-amd64" => "cass"
      end
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-arm64"
      sha256 "cc70f30105a2a242435954a78ead369820563f26420336c6b50beb361ef045eb" # bump-homebrew: SHA_LINUX_ARM64

      def install
        bin.install "cass-linux-arm64" => "cass"
      end
    else
      url "https://github.com/Cassandras-Edge/cass/releases/download/v#{version}/cass-linux-amd64"
      sha256 "2ddf3c74bd6afcfc2f46e65aa6f6e2ed1e28de302356835b2122be3e9f13a1bb" # bump-homebrew: SHA_LINUX_AMD64

      def install
        bin.install "cass-linux-amd64" => "cass"
      end
    end
  end

  test do
    assert_match "Usage", shell_output("#{bin}/cass --help")
  end
end
