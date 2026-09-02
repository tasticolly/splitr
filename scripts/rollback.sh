#!/bin/bash
# Откат на выбранную версию без потери текущей работы.
#
# Версия собирается в отдельном git worktree, а не переключением ветки:
# незакоммиченные правки остаются на месте, и вернуться обратно можно
# в любой момент, просто поставив свежую сборку заново.
set -euo pipefail

cd "$(dirname "$0")/.."
repo="$PWD"

if [ $# -lt 1 ]; then
	echo "Использование: $0 <версия>"
	echo "Доступные версии:"
	git tag -l --sort=-v:refname | head -10 | sed 's/^/  /'
	echo
	echo "Сейчас установлено: $(/usr/local/bin/splitr --version 2>/dev/null || echo неизвестно)"
	exit 1
fi

version="$1"
if ! git rev-parse "$version" > /dev/null 2>&1; then
	echo "Нет такой версии: $version" >&2
	exit 1
fi

work="$(mktemp -d)/splitr-$version"
cleanup() { git worktree remove --force "$work" 2> /dev/null || true; }
trap cleanup EXIT

echo "==> разворачиваю $version"
git worktree add --detach "$work" "$version" > /dev/null

echo "==> сборка"
(cd "$work" && go build -ldflags "-X github.com/tasticolly/splitr/internal/daemon.Version=$version" -o splitr ./cmd/splitr)

echo "==> установка (нужен sudo)"
sudo "$work/splitr" install

echo
echo "установлена версия $version"
echo "вернуться на актуальную: cd $repo && make update"
