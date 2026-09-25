#!/usr/bin/env python3
"""Checks that scripts/render-cask.py writes casks Homebrew 7 can tap.

Homebrew 7 loads every cask in a tap on each OS and arch it can simulate,
and refuses the whole tap when one of them fails. check() repeats its two
cask checks on the renderer's output:

- on macOS, each arch needs a url, unless the cask depends on Linux. This
  check ignores `depends_on arch:`.
- on Linux, each arch needs a sha256, unless the cask depends on macOS or
  its `depends_on arch:` leaves that arch out.

check() also returns the platforms the cask installs on. The tests compare
them with the platforms the archives are built for.

It reads only the Ruby the renderer writes. `brew readall` stays the
authority; this runs without Homebrew, on any host.

Usage: python3 scripts/render-cask-test.py
"""

import pathlib
import re
import subprocess
import sys
import tempfile
import unittest

RENDER = pathlib.Path(__file__).resolve().parent / "render-cask.py"
VERSION = "0.1.0-main.20260925161254.3448252"
TAG = f"channel-main-{VERSION}"
PLATFORMS = [(os, arch) for os in ("macos", "linux") for arch in ("arm", "intel")]
ARCH_DEP = {"arm": "arm64", "intel": "x86_64"}


def render(archives, extra=()):
    """Runs the renderer on empty archives and returns its exit code, output and cask."""
    with tempfile.TemporaryDirectory() as tmp:
        dist = pathlib.Path(tmp)
        for spec in archives:
            (dist / spec.split(":")[2]).write_bytes(b"")
        out = dist / "cask.rb"
        args = [
            sys.executable, str(RENDER),
            "--project", "hull", "--channel", "main", "--version", VERSION,
            "--repo", "brig-sh/hull", "--tag", TAG, "--dist", str(dist),
            "--desc", "Run microVMs", "--source", "the tip of main",
            *[f"--archive={a}" for a in archives], *extra,
            "--out", str(out),
        ]
        run = subprocess.run(args, capture_output=True, text=True)
        cask = out.read_text() if out.exists() else ""
    return run.returncode, run.stdout + run.stderr, cask


def evaluate(cask, os, arch):
    """Returns the url, sha256, OS and arch dependency the cask gives one platform."""
    found = {"url": None, "sha256": None, "os": None, "arch": None}
    blocks = []
    heredoc = False
    for line in cask.splitlines():
        s = line.strip()
        if heredoc:
            heredoc = s != "EOS"
            continue
        if s.endswith("<<~EOS"):
            heredoc = True
            continue
        block = re.fullmatch(r"(\S+).* do", s)
        if block:
            name = block.group(1)
            blocks.append(not name.startswith("on_") or name in (f"on_{os}", f"on_{arch}"))
            continue
        if s == "end":
            blocks.pop()
            continue
        if not all(blocks):
            continue
        m = re.fullmatch(r'(url|sha256) "(.*)"', s)
        if m:
            found[m.group(1)] = m.group(2)
        m = re.fullmatch(r"depends_on :(macos|linux)", s)
        if m:
            found["os"] = m.group(1)
        m = re.match(r"depends_on arch: +:(\w+)", s)
        if m:
            found["arch"] = m.group(1)
    return found


def check(cask):
    """Returns what Homebrew would refuse, and the platforms the cask installs on."""
    refused, installs = [], set()
    for os, arch in PLATFORMS:
        on = evaluate(cask, os, arch)
        if on["os"] not in (None, os):
            continue
        if os == "macos" and not on["url"]:
            refused.append(f"macOS on {arch}: no url")
            continue
        if on["arch"] not in (None, ARCH_DEP[arch]):
            continue
        if os == "linux" and not on["sha256"]:
            refused.append(f"Linux on {arch}: no sha256")
            continue
        installs.add((os, arch))
    return refused, installs


class RenderCaskTest(unittest.TestCase):
    def test_check_installs_only_where_homebrew_loads(self):
        # The layout the tap's hull@main.rb had: one nested url, no depends_on.
        cask = ('cask "x" do\n  version "1"\n\n  on_macos do\n    on_arm do\n'
                '      sha256 "0"\n      url "https://example.invalid/x-1.tar.gz"\n'
                '    end\n  end\nend\n')
        refused, installs = check(cask)
        self.assertEqual(refused, ["macOS on intel: no url", "Linux on arm: no sha256",
                                   "Linux on intel: no sha256"])
        self.assertEqual(installs, {("macos", "arm")})

    def test_hull_channel(self):
        # The arguments channel.yml passes.
        code, out, cask = render(
            [f"on_macos:on_arm:hull-{VERSION}-arm64.tar.gz"],
            ["--binary=hull", "--binary=vz-runner", "--binary=hvi",
             "--depends-formula=cosign", "--conflicts-with=experimental"])
        self.assertEqual(code, 0, out)
        refused, installs = check(cask)
        self.assertEqual(refused, [], cask)
        self.assertEqual(installs, {("macos", "arm")}, cask)

    def test_every_platform(self):
        code, out, cask = render([f"on_{os}:on_{arch}:{os}-{arch}.tar.gz" for os, arch in PLATFORMS])
        self.assertEqual(code, 0, out)
        refused, installs = check(cask)
        self.assertEqual(refused, [], cask)
        self.assertEqual(installs, set(PLATFORMS), cask)

    def test_macos_both_arches(self):
        code, out, cask = render(["on_macos:on_arm:arm.tar.gz", "on_macos:on_intel:intel.tar.gz"])
        self.assertEqual(code, 0, out)
        refused, installs = check(cask)
        self.assertEqual(refused, [], cask)
        self.assertEqual(installs, {("macos", "arm"), ("macos", "intel")}, cask)

    def test_url_names_the_version(self):
        # brew audit takes a url without #{version} for an unversioned one.
        code, out, cask = render([f"on_macos:on_arm:hull-{VERSION}-arm64.tar.gz"])
        self.assertEqual(code, 0, out)
        self.assertEqual(evaluate(cask, "macos", "arm")["url"],
                         "https://github.com/brig-sh/hull/releases/download/"
                         "channel-main-#{version}/hull-#{version}-arm64.tar.gz")

    def test_url_names_only_the_version_fields(self):
        # "6" is also in "arm64", which has to stay as it is.
        code, out, cask = render(["on_macos:on_arm:hull-6-arm64.tar.gz"],
                                 ["--version=6", "--tag=channel-main-6"])
        self.assertEqual(code, 0, out)
        self.assertEqual(evaluate(cask, "macos", "arm")["url"],
                         "https://github.com/brig-sh/hull/releases/download/"
                         "channel-main-#{version}/hull-#{version}-arm64.tar.gz")

    def test_refuses_an_os_with_one_arch(self):
        code, out, _ = render(["on_macos:on_arm:arm.tar.gz", "on_linux:on_arm:linux-arm.tar.gz",
                               "on_linux:on_intel:linux-intel.tar.gz"])
        self.assertNotEqual(code, 0)
        self.assertIn("no archive for on_macos on_intel", out)

    def test_linux_single_archive(self):
        code, out, cask = render(["on_linux:on_intel:linux-amd64.tar.gz"])
        self.assertEqual(code, 0, out)
        # arch alone takes no padding; brew style calls it extra spacing.
        self.assertIn("\n  depends_on arch: :x86_64\n  depends_on :linux\n", cask)
        refused, installs = check(cask)
        self.assertEqual(refused, [], cask)
        self.assertEqual(installs, {("linux", "intel")}, cask)


if __name__ == "__main__":
    unittest.main()
