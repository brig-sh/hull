#!/bin/sh
# Install hull-guest-agent in a running macOS guest over SSH.
#
#   scripts/macos-guest-provision.sh <agent-binary> <ssh-destination> [ssh-option...]
#
# The agent is what `hull exec` talks to. A guest image has to carry it, and
# until hull builds its own images this script adds it to an existing one, eg.
# a Tart VM. The SSH user needs sudo without a password prompt.
set -eu

if [ $# -lt 2 ]; then
	echo "usage: $0 <agent-binary> <ssh-destination> [ssh-option...]" >&2
	exit 2
fi
agent=$1
dest=$2
shift 2

label=sh.brig.hull-guest-agent
plist=$(dirname "$0")/../packaging/macos-guest/$label.plist

ssh "$@" "$dest" 'cat > /tmp/hull-guest-agent' < "$agent"
ssh "$@" "$dest" "cat > /tmp/$label.plist" < "$plist"
ssh "$@" "$dest" "
	set -e
	sudo install -d -m 755 /usr/local/bin
	sudo install -m 755 -o root -g wheel /tmp/hull-guest-agent /usr/local/bin/hull-guest-agent
	sudo install -m 644 -o root -g wheel /tmp/$label.plist /Library/LaunchDaemons/$label.plist
	rm -f /tmp/hull-guest-agent /tmp/$label.plist
	sudo launchctl bootout system/$label 2>/dev/null || true
	sudo launchctl bootstrap system /Library/LaunchDaemons/$label.plist
	sudo launchctl print system/$label | grep -E 'state =|pid ='
"
