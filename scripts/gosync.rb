#typed: true
# frozen_string_literal: true

# This file is auto-generated from Gosync release configuration
# Do not edit manually

class Gosync < Formula
  desc "Adaptive parallel file transfer tool with selectable transport backends"
  homepage "https://gitlab.zarquon.space/meganerd/gosync"
  version "1.0.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://gitlab.zarquon.space/meganerd/gosync/-/releases/1.0.0/downloads/gosync-darwin-arm64"
      sha256 "PLACEHOLDER_ARM64_SHA256"
    else
      url "https://gitlab.zarquon.space/meganerd/gosync/-/releases/1.0.0/downloads/gosync-darwin-amd64"
      sha256 "PLACEHOLDER_AMD64_SHA256"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://gitlab.zarquon.space/meganerd/gosync/-/releases/1.0.0/downloads/gosync-linux-arm64"
      sha256 "PLACEHOLDER_LINUX_ARM64_SHA256"
    else
      url "https://gitlab.zarquon.space/meganerd/gosync/-/releases/1.0.0/downloads/gosync-linux-amd64"
      sha256 "PLACEHOLDER_LINUX_AMD64_SHA256"
    end
  end

  def install
    bin.install "gosync"
  end

  test do
    system "#{bin}/gosync", "--version"
  end
end
