#!/usr/bin/env python3
"""Render a Homebrew cask, the stable one or one prerelease channel's.

Without --channel it writes the stable cask for a release. GoReleaser's cask
template cannot write hull's: it nests each archive's url under on_macos and
on_arm, and Homebrew 7 refuses a tap whose cask has no url for macOS on
Intel. A channel cask cannot come from GoReleaser either. GoReleaser renders a
cask for the version it is releasing, and a channel has no version of its own
-- it is whatever main, or one named ref, happens to be.

One renderer for every hull cask. The alternative is several hand-maintained
casks that drift from each other. brig-sh/brig keeps its own copy for brig's
channel casks. The two have diverged: only this one puts a single archive at
the top level and writes the stable cask.
"""

import argparse
import hashlib
import pathlib
import sys

# The os stanzas a cask can nest, in the order they must appear.
OS_ORDER = ["on_macos", "on_linux"]
# The arch stanzas, and the `depends_on arch:` value for each.
ARCHES = {"on_arm": "arm64", "on_intel": "x86_64"}


def parse_archives(specs):
    """Read the --archive values: <os stanza>:<arch stanza>:<file name>.

    Explicit rather than derived from a platform table, because the two
    projects do not ship the same set. brig builds four platforms and names
    the archive after both the OS and the arch; hull is Apple Silicon only and
    names it after the arch alone.
    """
    out = []
    for spec in specs:
        parts = spec.split(":")
        if len(parts) != 3:
            sys.exit(f"--archive {spec!r} is not <os stanza>:<arch stanza>:<file name>")
        if parts[0] not in OS_ORDER or parts[1] not in ARCHES:
            sys.exit(f"--archive {spec!r}: the os stanza is one of {OS_ORDER}"
                     f" and the arch stanza one of {list(ARCHES)}")
        out.append(tuple(parts))
    return sorted(out, key=lambda a: (OS_ORDER.index(a[0]), a[1]))


def requirements(archives):
    """The OS and the arch the cask has to depend on, or None for either.

    Homebrew 7 loads every cask in a tap on each OS and arch it can simulate,
    and refuses the whole tap when one of them fails. On macOS every arch
    needs a url, whatever `depends_on arch:` says. On Linux every arch needs a
    sha256, unless the cask depends on macOS or leaves that arch out.

    So a single archive goes at the top level, where every OS and arch sees
    it, and the cask depends on its OS and its arch. More than one are nested,
    and each OS they name needs an archive for both arches. Archives for one
    OS alone make the cask depend on that OS.
    """
    seen = {}
    for os_stanza, arch_stanza, name in archives:
        if arch_stanza in seen.setdefault(os_stanza, set()):
            sys.exit(f"two archives for {os_stanza} {arch_stanza}, the second is {name}")
        seen[os_stanza].add(arch_stanza)
    if len(archives) > 1:
        for os_stanza, arches in seen.items():
            for arch_stanza in ARCHES:
                if arch_stanza not in arches:
                    sys.exit(f"no archive for {os_stanza} {arch_stanza}: Homebrew would"
                             " find no url there and refuse the whole tap")
    os_dep = next(iter(seen)).removeprefix("on_") if len(seen) == 1 else None
    arch_dep = ARCHES[archives[0][1]] if len(archives) == 1 else None
    return os_dep, arch_dep


def platform_stanzas(args, archives):
    """The url and sha256 for each archive this build produced.

    An archive that is not there stops the render. A cask silently missing one
    platform installs on the others and reports nothing on that one.
    """
    dist = pathlib.Path(args.dist)
    out = []
    for os_stanza, arch_stanza, name in archives:
        archive = dist / name
        if not archive.exists():
            sys.exit(f"{archive} was not built, so the cask would be missing {name}")
        # brew audit takes a url without #{version} for an unversioned one,
        # and then wants `sha256 :no_check`. The version is substituted in the
        # tag and in the file name only, once in each, so a version such as
        # "6" leaves the "64" of "arm64" as it is.
        tag = args.tag.replace(args.version, "#{version}", 1)
        name = archive.name.replace(args.version, "#{version}", 1)
        out.append((
            os_stanza,
            arch_stanza,
            f"https://github.com/{args.repo}/releases/download/{tag}/{name}",
            hashlib.sha256(archive.read_bytes()).hexdigest(),
        ))
    return out


def depends_on(args, os_dep, arch_dep):
    """The depends_on stanzas, or nothing when this cask needs none."""
    groups = []
    if arch_dep:
        groups.append(("arch:", [f":{arch_dep}"], False))
    if args.depends_cask:
        groups.append(("cask:", args.depends_cask, True))
    if args.depends_formula:
        groups.append(("formula:", args.depends_formula, True))
    # brew style aligns the values of the keys that are there, and calls any
    # other spacing extra.
    width = max((len(label) for label, _, _ in groups), default=0)
    lines = []
    for i, (label, names, quote) in enumerate(groups):
        head = "  depends_on " if i == 0 else "             "
        tail = "," if i < len(groups) - 1 else ""
        label = label.ljust(width)
        if not quote and len(names) == 1:
            # A symbol, and a single one: written inline the way hull's own
            # cask writes it rather than as a one-element array.
            lines.append(f"{head}{label} {names[0]}{tail}")
            continue
        lines.append(f"{head}{label} [")
        lines += [f'               "{n}",' for n in names]
        lines.append("             ]" + tail)
    # The bare form Homebrew asks for, on a line of its own. brew style sorts
    # it after the keyword form above.
    if os_dep:
        lines.append(f"  depends_on :{os_dep}")
    return lines


def generator():
    """The path this script was invoked by, for the DO NOT EDIT banner.

    Read rather than written down: the two repositories keep it in different
    directories, and a banner that says DO NOT EDIT should name a path that
    resolves in the repository it was written into.
    """
    path = pathlib.Path(sys.argv[0])
    try:
        return str(path.relative_to(pathlib.Path.cwd()))
    except ValueError:
        return str(path.name if path.is_absolute() else path)


def render(args):
    archives = parse_archives(args.archive)
    os_dep, arch_dep = requirements(archives)
    stanzas = platform_stanzas(args, archives)

    lines = [
        "# frozen_string_literal: true",
        "",
        f"# Generated by {generator()} in {args.repo}. DO NOT EDIT.",
    ]
    if args.channel:
        lines += [
            "#",
            f"# The {args.channel} channel: built from {args.source}. It is not a",
            "# release, it moves without notice, and it is not what",
            f"# `brew install --cask {args.project}` gives you.",
            f'cask "{args.project}@{args.channel}" do',
        ]
    else:
        lines.append(f'cask "{args.project}" do')
    lines.append(f'  version "{args.version}"')

    if len(stanzas) == 1:
        _, _, url, digest = stanzas[0]
        lines += [
            f'  sha256 "{digest}"',
            "",
            f'  url "{url}"',
        ]
    else:
        lines.append("")
        last_os = None
        for os_stanza, arch_stanza, url, digest in stanzas:
            if os_stanza != last_os:
                if last_os is not None:
                    lines.append("  end")
                lines.append(f"  {os_stanza} do")
                last_os = os_stanza
            lines += [
                f"    {arch_stanza} do",
                f'      sha256 "{digest}"',
                f'      url "{url}"',
                "    end",
            ]
        lines += ["  end", ""]

    lines += [
        f'  name "{args.project} ({args.channel})"' if args.channel else f'  name "{args.project}"',
        f'  desc "{args.desc}"',
        f'  homepage "https://github.com/{args.repo}"',
        "",
        "  livecheck do",
    ]
    if args.channel:
        lines.append('    skip "Channel build, not a release."')
    else:
        # The newest release that is not a prerelease, which is what a stable
        # tag publishes and a release candidate does not.
        lines += ["    url :url", "    strategy :github_latest"]
    lines += ["  end", ""]

    # Every cask of a project installs the same binaries at the same paths, so
    # brew has to be told they cannot coexist. Homebrew reads only the
    # conflicts of the cask being installed, so each cask names all the
    # others. Without this the second install fails on a link brew did not
    # expect to find already there, and the message says nothing about the
    # cause.
    others = [f"{args.project}@{c}" for c in args.conflicts_with]
    if args.channel:
        others.append(args.project)
    group = []
    if len(others) == 1:
        # brew style refuses a one-element array here.
        group.append(f'  conflicts_with cask: "{others[0]}"')
    elif others:
        group.append("  conflicts_with cask: [")
        group += [f'    "{other}",' for other in sorted(others)]
        group.append("  ]")
    # conflicts_with and depends_on are one stanza group for brew style: no
    # blank line between them, and one after whichever comes last.
    group += depends_on(args, os_dep, arch_dep)
    if group:
        lines += group + [""]

    for binary in args.binary:
        lines.append(f'  binary "{binary}"')
    # bash, fish, zsh: brew style enforces this order, not the order the
    # flags were written in.
    for shell in ("bash", "fish", "zsh"):
        if shell in args.completion:
            lines.append(f'  {shell}_completion "completions/{args.project}.{shell}"')
    if args.binary or args.completion:
        lines.append("")

    if args.channel:
        owner = args.repo.split("/")[0]
        caveats = [
            f"This is the {args.channel} channel, built from {args.source}.",
            "It is not a release: it can break, and it moves without notice.",
            "",
            f"The supported build:  brew install --cask {owner}/brig/{args.project}",
        ]
    elif args.caveats_file:
        path = pathlib.Path(args.caveats_file)
        if not path.is_file():
            sys.exit(f"{path} was not found, so the cask would have no caveats")
        caveats = path.read_text().rstrip("\n").split("\n")
    else:
        caveats = []
    if caveats:
        lines.append("  caveats <<~EOS")
        lines += [f"    {c}".rstrip() for c in caveats]
        lines.append("  EOS")
    elif lines[-1] == "":
        lines.pop()
    lines += ["end", ""]
    return "\n".join(lines)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--project", required=True, help="brig or hull")
    p.add_argument("--channel", default="",
                   help="main or experimental; without it, the stable cask")
    p.add_argument("--version", required=True)
    p.add_argument("--repo", required=True, help="owner/name the release lives in")
    p.add_argument("--tag", required=True, help="the tag of the release holding the assets")
    p.add_argument("--dist", default="dist")
    p.add_argument("--archive", action="append", default=[], required=True,
                   help="<os stanza>:<arch stanza>:<file name>, repeatable")
    p.add_argument("--desc", required=True)
    p.add_argument("--source", default="",
                   help="what a channel build came from, for its caveat")
    p.add_argument("--caveats-file", default="",
                   help="the stable cask's caveats, as plain text")
    p.add_argument("--binary", action="append", default=[])
    p.add_argument("--completion", action="append", default=[])
    p.add_argument("--depends-cask", action="append", default=[])
    p.add_argument("--depends-formula", action="append", default=[])
    p.add_argument("--conflicts-with", action="append", default=[],
                   help="a channel of this project, repeatable")
    p.add_argument("--out", required=True)
    args = p.parse_args()
    if args.channel and not args.source:
        p.error("a channel cask needs --source")
    if args.channel and args.caveats_file:
        p.error("--caveats-file is for the stable cask; a channel writes its own")
    pathlib.Path(args.out).write_text(render(args))
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
