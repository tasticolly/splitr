#!/bin/bash
# Выпуск новой версии: проверка, тег, подсказка как откатиться.
#
# Тег нужен не для красоты: когда после обновления что-то сломалось, вопрос
# всегда один — «а на прошлой версии работало?». Без тегов ответить на него
# можно только копанием в истории.
set -euo pipefail

cd "$(dirname "$0")/.."

usage() {
	echo "Использование: $0 <версия> [описание]"
	echo "  $0 v0.3.0 'подменю в строке меню'"
	echo
	echo "Текущая версия: $(git describe --tags --always --dirty 2>/dev/null || echo нет)"
	echo "Все версии:"
	git tag -l --sort=-v:refname | head -10 | sed 's/^/  /'
	exit 1
}

[ $# -ge 1 ] || usage
version="$1"
message="${2:-}"

case "$version" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*)
	echo "Версия должна выглядеть как v1.2.3, получено: $version" >&2
	exit 1
	;;
esac

if git rev-parse "$version" > /dev/null 2>&1; then
	echo "Тег $version уже существует" >&2
	exit 1
fi

if [ -n "$(git status --porcelain)" ]; then
	echo "В рабочем дереве есть незакоммиченные изменения:" >&2
	git status --short >&2
	echo >&2
	echo "Тег на грязном дереве бесполезен: по нему нельзя воспроизвести сборку." >&2
	exit 1
fi

echo "==> проверки"
gofmt -l . | tee /tmp/splitr-fmt.txt
[ ! -s /tmp/splitr-fmt.txt ] || { echo "gofmt: файлы выше не отформатированы" >&2; exit 1; }
go vet ./...
go test ./...

echo "==> тег $version"
if [ -n "$message" ]; then
	git tag -a "$version" -m "$message"
else
	git tag -a "$version" -m "$version"
fi

echo
echo "готово: $version"
echo "  собрать и поставить:  make update"
echo "  вернуться назад:      ./scripts/rollback.sh <версия>"
echo "  список версий:        git tag -l --sort=-v:refname"
