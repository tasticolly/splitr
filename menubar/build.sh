#!/bin/bash
# Сборка SplitR.app одной командой, без Xcode-проекта.
#
# Xcode-проект здесь был бы лишним: приложение — это семь файлов и системные
# фреймворки. swiftc собирает их напрямую, а бандл проще сложить руками,
# чем держать в репозитории .pbxproj, который никто не редактирует осмысленно.
set -euo pipefail

cd "$(dirname "$0")"

APP_NAME="SplitR"
BUILD_DIR="build"
APP="$BUILD_DIR/$APP_NAME.app"
MACOS_DIR="$APP/Contents/MacOS"
RES_DIR="$APP/Contents/Resources"

# Минимальная поддерживаемая система. 13.0 — там уже есть и палитра для
# SF Symbols, и современный UserNotifications; ниже опускаться незачем.
DEPLOY_TARGET="13.0"
ARCH="$(uname -m)"
TARGET="${ARCH}-apple-macos${DEPLOY_TARGET}"

SDK="$(xcrun --show-sdk-path --sdk macosx)"

echo "==> cleaning $APP"
rm -rf "$APP"
mkdir -p "$MACOS_DIR" "$RES_DIR"

echo "==> compiling ($TARGET)"
# -O: приложение висит в памяти всё время сессии, отладочная сборка тут не нужна.
# Файлы перечисляем целиком: у swiftc нет инкрементальной сборки без проекта,
# зато весь модуль компилируется за пару секунд.
xcrun swiftc \
	-sdk "$SDK" \
	-target "$TARGET" \
	-O \
	-framework AppKit \
	-framework UserNotifications \
	-o "$MACOS_DIR/$APP_NAME" \
	Sources/*.swift

echo "==> bundling"
cp Resources/Info.plist "$APP/Contents/Info.plist"
printf 'APPL????' > "$APP/Contents/PkgInfo"

# Ad-hoc подпись обязательна, а не «для красоты»: без валидной подписи
# UserNotifications отказывается регистрировать приложение, и уведомления
# о падении туннеля молча не приходят.
echo "==> signing (ad-hoc)"
codesign --force --sign - --timestamp=none "$APP" >/dev/null

echo "==> verifying"
codesign --verify --deep --strict "$APP"
/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$APP/Contents/Info.plist" >/dev/null
test -x "$MACOS_DIR/$APP_NAME"

echo "done: $(pwd)/$APP"
echo "run it: open $(pwd)/$APP"
echo "install into /Applications and at login: ./install.sh"
