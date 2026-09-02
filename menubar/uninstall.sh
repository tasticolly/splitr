#!/bin/bash
# Снять SplitR.app: выгрузить агент, убрать плист и бандл.
# Демон splitr при этом не трогаем — это отдельная сущность,
# и убивать защиту трафика заодно с иконкой было бы сюрпризом.
set -euo pipefail

APP_NAME="SplitR"
DEST="/Applications/$APP_NAME.app"
LABEL="com.splitr.menubar"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
DOMAIN="gui/$(id -u)"

echo "==> removing start at login"
# Снимаем оба механизма: какой из них сработал при установке, зависит от того,
# приняла ли система ad-hoc подпись, и здесь это уже неважно.
if [ -x "$DEST/Contents/MacOS/$APP_NAME" ]; then
	"$DEST/Contents/MacOS/$APP_NAME" --unregister-login-item 2>/dev/null || true
fi
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
rm -f "$PLIST"

echo "==> stopping the app"
pkill -x "$APP_NAME" 2>/dev/null || true

echo "==> deleting $DEST"
rm -rf "$DEST"

echo "done. The SplitR daemon keeps running — remove it with: sudo splitr uninstall"
