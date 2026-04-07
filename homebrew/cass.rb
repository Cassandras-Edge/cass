class Cass < Formula
  include Language::Python::Virtualenv

  desc "Cassandra platform CLI — cookie sync, MCP key management"
  homepage "https://github.com/digibugcat/cass"
  url "https://github.com/digibugcat/cass/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "PLACEHOLDER"
  license "MIT"

  depends_on "python@3.13"
  depends_on "yt-dlp" => :recommended

  resource "click" do
    url "https://files.pythonhosted.org/packages/96/d3/f04c7bfcf5c1862a2a5b845c6b2b360488cf47af55dfa79c98f6a6bf98b5/click-8.1.7.tar.gz"
    sha256 "ca9853ad459e787e2192211578cc907e7594e294c7ccc834310722b41b9ca6de"
  end

  resource "httpx" do
    url "https://files.pythonhosted.org/packages/06/94/82699a10bca87e5a0b34816c295d2f5de3538e tried/httpx-0.28.1.tar.gz"
    sha256 "PLACEHOLDER"
  end

  def install
    virtualenv_install_with_resources
  end

  test do
    assert_match "Usage", shell_output("#{bin}/cass --help")
  end
end
