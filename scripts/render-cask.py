#!/usr/bin/env python3
"""Render a Homebrew cask for one prerelease channel.

The stable cask is GoReleaser's, generated on a tag and opened as a pull
request against the tap. A channel cask cannot come from there: GoReleaser
renders a cask for the version it is releasing, and a channel has no version
of its own -- it is whatever main, or one named ref, happens to be.

One renderer for every channel and both projects. The alternative is four
hand-maintained casks that drift from each other and from the stable one.
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
    return lines + [""] if lines else []


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
        "#",
        f"# The {args.channel} channel: built from {args.source}. It is not a",
        "# release, it moves without notice, and it is not what",
        f"# `brew install --cask {args.project}` gives you.",
        f'cask "{args.project}@{args.channel}" do',
        f'  version "{args.version}"',
    ]

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
        f'  name "{args.project} ({args.channel})"',
        f'  desc "{args.desc}"',
        f'  homepage "https://github.com/{args.repo}"',
        "",
        "  livecheck do",
        '    skip "Channel build, not a release."',
        "  end",
        "",
    ]

    # Every channel cask installs the same binaries at the same paths as the
    # stable one, so brew has to be told they cannot coexist. Without this the
    # second install fails on a link brew did not expect to find already
    # there, and the message says nothing about the cause.
    lines.append("  conflicts_with cask: [")
    for other in [args.project] + [f"{args.project}@{c}" for c in args.conflicts_with]:
        lines.append(f'    "{other}",')
    lines.append("  ]")

    # No blank line before depends_on: brew style groups the two together and
    # a separator between them is an offence.
    on = depends_on(args, os_dep, arch_dep)
    if not on:
        # conflicts_with and depends_on are one stanza group, so the blank line
        # that separates the group from what follows belongs to whichever of
        # them comes last. With no dependencies it is conflicts_with.
        on = [""]
    lines += on

    for binary in args.binary:
        lines.append(f'  binary "{binary}"')
    # bash, fish, zsh: brew style enforces this order, not the order the
    # flags were written in.
    for shell in ("bash", "fish", "zsh"):
        if shell in args.completion:
            lines.append(f'  {shell}_completion "completions/{args.project}.{shell}"')
    if args.binary or args.completion:
        lines.append("")

    owner = args.repo.split("/")[0]
    lines += [
        "  caveats <<~EOS",
        f"    This is the {args.channel} channel, built from {args.source}.",
        "    It is not a release: it can break, and it moves without notice.",
        "",
        f"    The supported build:  brew install --cask {owner}/brig/{args.project}",
        "  EOS",
        "end",
        "",
    ]
    return "\n".join(lines)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--project", required=True, help="brig or hull")
    p.add_argument("--channel", required=True, help="main or experimental")
    p.add_argument("--version", required=True)
    p.add_argument("--repo", required=True, help="owner/name the release lives in")
    p.add_argument("--tag", required=True, help="the tag of the release holding the assets")
    p.add_argument("--dist", default="dist")
    p.add_argument("--archive", action="append", default=[], required=True,
                   help="<os stanza>:<arch stanza>:<file name>, repeatable")
    p.add_argument("--desc", required=True)
    p.add_argument("--source", required=True, help="what this build came from, for the caveat")
    p.add_argument("--binary", action="append", default=[])
    p.add_argument("--completion", action="append", default=[])
    p.add_argument("--depends-cask", action="append", default=[])
    p.add_argument("--depends-formula", action="append", default=[])
    p.add_argument("--conflicts-with", action="append", default=[])
    p.add_argument("--out", required=True)
    args = p.parse_args()
    pathlib.Path(args.out).write_text(render(args))
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
