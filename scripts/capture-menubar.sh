#!/bin/bash
# Screenshot of the SplitR menu, for the README.
#
# The menu is a window that exists only while it is open, so the shot has to be
# taken with it held open. Clicking the status item toggles it, so the script
# asks the accessibility API whether the menu is already showing instead of
# clicking blindly, then captures exactly the menu's own bounds: anything wider
# would drag whatever happens to be on the desktop into the picture.
#
# Needs the screen unlocked, and the terminal running this needs Accessibility
# and Screen Recording permission (System Settings > Privacy & Security).
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="${1:-docs/menubar.png}"

if ! pgrep -qf 'SplitR.app/Contents/MacOS/SplitR'; then
	echo "SplitR.app is not running; start it first" >&2
	exit 1
fi

ax() { osascript -e "tell application \"System Events\" to tell process \"SplitR\" to $1" 2>/dev/null; }

# A menu that is already open would be closed by a click, so close it first and
# start from a known state.
if [ "$(ax 'get value of attribute "AXSelected" of menu bar item 1 of menu bar 1')" = "true" ]; then
	osascript -e 'tell application "System Events" to key code 53'
	sleep 0.4
fi

ax 'click menu bar item 1 of menu bar 1' > /dev/null
sleep 1.2

# The accessibility API reports no geometry for a status item's menu (it comes
# back 0x0), so the region is derived from the icon instead: the menu drops from
# the top and opens to the right of it. 380x315 covers it with a little air.
pos=$(ax 'get position of menu bar item 1 of menu bar 1')
ix=${pos%%,*}
x=$((ix - 15))
[ "$x" -lt 0 ] && x=0

if [ "$(ax 'get value of attribute "AXSelected" of menu bar item 1 of menu bar 1')" != "true" ]; then
	osascript -e 'tell application "System Events" to key code 53' || true
	echo "the menu did not open; is the screen unlocked and Accessibility granted?" >&2
	exit 1
fi

echo "==> icon at $ix, capturing ${x},0 380x315"
screencapture -x -R "${x},0,380,315" "$OUT"

osascript -e 'tell application "System Events" to key code 53'
echo "==> wrote $OUT"
