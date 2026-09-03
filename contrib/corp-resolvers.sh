#!/bin/bash
# Point macOS at the corporate resolvers for the internal domains only.
#
# This is the other half of the profile's dns_servers setting. That setting
# tells sshuttle which DNS traffic to carry through the tunnel; this tells
# macOS which names to send there in the first place. Without it the system
# asks its default resolver about internal names and gets nothing back, and
# the tunnelled resolvers are never addressed at all.
#
# Each file under /etc/resolver is named after a domain and applies to that
# domain and everything under it. Names outside the list keep using the
# default resolver, so public DNS never leaves the direct path.
#
#   ./corp-resolvers.sh install 10.73.16.4,10.73.0.23 corp.example.com ...
#   ./corp-resolvers.sh remove   corp.example.com ...
#   ./corp-resolvers.sh list
set -euo pipefail

usage() {
	sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
}

case "${1:-}" in
install)
	shift
	[ $# -ge 2 ] || usage
	IFS=',' read -ra servers <<< "$1"
	shift
	for domain in "$@"; do
		file="/etc/resolver/$domain"
		{
			echo "# written by splitr contrib/corp-resolvers.sh"
			for s in "${servers[@]}"; do echo "nameserver $s"; done
		} | sudo tee "$file" > /dev/null
		echo "==> $file"
	done
	sudo mkdir -p /etc/resolver
	sudo dscacheutil -flushcache
	sudo killall -HUP mDNSResponder 2> /dev/null || true
	echo "done; check one with: scutil --dns | grep -A3 'domain   : $1'"
	;;
remove)
	shift
	[ $# -ge 1 ] || usage
	for domain in "$@"; do
		sudo rm -f "/etc/resolver/$domain" && echo "==> removed /etc/resolver/$domain"
	done
	sudo dscacheutil -flushcache
	sudo killall -HUP mDNSResponder 2> /dev/null || true
	;;
list)
	ls -1 /etc/resolver 2> /dev/null || echo "no per-domain resolvers are configured"
	;;
*)
	usage
	;;
esac
