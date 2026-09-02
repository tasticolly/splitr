#!/bin/bash
# Установка SplitR.app в /Applications и автозапуск при логине.
#
# Никакого sudo: приложение только читает API демона, привилегий ему не нужно.
#
# Автозапуск делаем в два хода. Сначала пробуем объект входа (SMAppService,
# macOS 13+) — это штатный способ: приложение видно в «Настройки → Основные →
# Объекты входа», и пользователь может выключить его там, а не искать плист.
# Если не вышло — а с ad-hoc подписью это вполне вероятно, у неё нет
# устойчивого удостоверения для launchd — кладём LaunchAgent, как раньше.
set -euo pipefail

cd "$(dirname "$0")"

APP_NAME="SplitR"
SRC="build/$APP_NAME.app"
DEST="/Applications/$APP_NAME.app"
LABEL="com.splitr.menubar"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
DOMAIN="gui/$(id -u)"

if [ ! -d "$SRC" ]; then
	echo "==> no bundle yet, building"
	./build.sh
fi

echo "==> installing $DEST"
# Старую копию сносим целиком: cp -R поверх живого бандла оставляет мусор
# от прошлой версии и ломает подпись.
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
if pgrep -x "$APP_NAME" >/dev/null; then
	echo "    stopping the running copy"
	pkill -x "$APP_NAME" || true
	sleep 1
fi
rm -rf "$DEST"
cp -R "$SRC" "$DEST"
# Карантин ломает и объект входа, и обычный запуск: launchd отказывается
# исполнять программу с com.apple.quarantine. Снимаем сразу.
xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true
# Регистрируем бандл в LaunchServices, иначе система может не знать о нём
# при первом обращении SMAppService.
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister \
	-f "$DEST" 2>/dev/null || true

echo "==> start at login"
if "$DEST/Contents/MacOS/$APP_NAME" --register-login-item; then
	# Объект входа взлетел — старый LaunchAgent надо убрать, иначе приложение
	# будет запускаться дважды.
	if [ -f "$PLIST" ]; then
		echo "    removing the old LaunchAgent $PLIST"
		rm -f "$PLIST"
	fi
	AUTOSTART="login item (System Settings → General → Login Items)"
	echo "==> launching"
	open -a "$DEST"
else
	echo "    login item unavailable, installing LaunchAgent $PLIST"
	mkdir -p "$HOME/Library/LaunchAgents"
	cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$DEST/Contents/MacOS/$APP_NAME</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<!-- KeepAlive только при аварийном выходе: пункт «Выход» в меню должен
	     действительно выключать приложение, а не перезапускать его launchd'ом. -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardErrorPath</key>
	<string>/tmp/$LABEL.log</string>
</dict>
</plist>
PLIST_EOF
	# bootout перед bootstrap: launchd отказывается загружать уже загруженный
	# лейбл, а при обновлении приложения перезагрузить агент нужно обязательно.
	launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
	launchctl bootstrap "$DOMAIN" "$PLIST"
	launchctl kickstart -k "$DOMAIN/$LABEL"
	AUTOSTART="LaunchAgent $PLIST"
fi

echo
echo "done."
echo "  app:        $DEST"
echo "  at login:   $AUTOSTART"
echo "  log:        $HOME/Library/Logs/SplitR.log"
echo "  uninstall:  ./uninstall.sh"
echo
echo "The icon appears in the menu bar. If you cannot see it, check the log:"
echo "  tail -20 $HOME/Library/Logs/SplitR.log"
echo "the app records there what happened to it (in particular whether it ended up"
echo "under the screen notch because the menu bar is full)."
