#!/bin/bash
# Screenshot of the SplitR menu, for the README.
#
# The menu is a window that only exists while it is open, so the shot has to be
# taken with the menu held open: this clicks the status item, waits, captures a
# region anchored on the icon, and clicks again to close it.
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

pos=$(osascript <<'APPLESCRIPT'
tell application "System Events"
	tell process "SplitR"
		set b to menu bar item 1 of menu bar 1
		set {x, y} to position of b
		set {w, h} to size of b
		return (x as text) & " " & (y as text) & " " & (w as text) & " " & (h as text)
	end tell
end tell
APPLESCRIPT
)
read -r ix iy iw ih <<< "$pos"
echo "==> status item at ${ix},${iy} (${iw}x${ih})"

osascript -e 'tell application "System Events" to tell process "SplitR" to click menu bar item 1 of menu bar 1'
sleep 1.2

# The menu hangs below the icon and extends to the left of it. 460 wide and 640
# tall covers the whole thing with room to spare; crop the result afterwards.
x=$((ix - 400))
[ "$x" -lt 0 ] && x=0
screencapture -x -R "${x},0,460,640" "$OUT"

osascript -e 'tell application "System Events" to key code 53' # Esc, closes the menu

echo "==> wrote $OUT"
