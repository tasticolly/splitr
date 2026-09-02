#!/bin/bash
# Запуск нативных pf-тестов splitr одной командой:
#
#     sudo test/pfe2e/run.sh
#
# Скрипт делает три вещи:
#   1. гоняет тесты разбора правил (root не нужен, их же гоняет CI);
#   2. гоняет e2e-тест под root — он временно подменяет главный набор правил
#      pf, чтобы подключить два тестовых якоря;
#   3. в любом случае возвращает pf в исходное состояние (`pfctl -f /etc/pf.conf`)
#      и проверяет, что сеть жива.
#
# /etc/pf.conf НЕ редактируется: временный набор правил лежит во временном
# файле, а откат — это просто перезагрузка штатного конфига.

set -uo pipefail

PFCTL=/sbin/pfctl
PF_CONF=/etc/pf.conf
E2E_ANCHOR=splitr_e2e
SSH_ANCHOR=splitr_e2e_sshuttle
TIMEOUT=5m

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$REPO_ROOT" || exit 1

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[33m%s\033[0m\n' "$*"; }

if [[ "$(uname -s)" != "Darwin" ]]; then
	red "Тест имеет смысл только на macOS: в других системах нет pf."
	exit 1
fi

if [[ "$(id -u)" -ne 0 ]]; then
	red "Нужен root: тест меняет главный набор правил pf."
	echo "Запустите: sudo $0"
	exit 1
fi

# Тулчейн зовём от имени вызвавшего пользователя: сборка под root оставила бы
# в его кэше Go root-овые файлы, после чего обычный `go build` начнёт падать.
run_as_user() {
	if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
		sudo -u "$SUDO_USER" -H "$@"
	else
		"$@"
	fi
}

GO_BIN=${GO_BIN:-$(run_as_user bash -lc 'command -v go' 2>/dev/null)}
if [[ -z "$GO_BIN" || ! -x "$GO_BIN" ]]; then
	red "Не найден бинарь go. Укажите его явно: sudo GO_BIN=/opt/homebrew/bin/go $0"
	exit 1
fi

bold "Что сейчас будет сделано"
cat <<'EOF'
  * снимется снимок текущего состояния pf (правила, якоря, статус);
  * во временный файл соберётся копия /etc/pf.conf плюс два тестовых якоря
    (splitr_e2e и splitr_e2e_sshuttle) и загрузится в pf;
  * тест несколько раз попробует соединиться с достижимым адресом вне машины,
    включая и выключая блокировку;
  * в конце выполнится `pfctl -f /etc/pf.conf` — pf вернётся ровно к тому,
    что описано в вашем штатном конфиге.

  ВАЖНО: перезагрузка главного набора правил снимает якоря, добавленные
  динамически (sshuttle, Cisco AnyConnect). Поэтому во время теста не должно
  быть поднятого туннеля или VPN — тест это проверяет и откажется работать.
EOF
echo

if [[ "${1:-}" != "-y" && "${SPLITR_E2E_YES:-}" != "1" ]]; then
	read -r -p "Продолжить? [y/N] " answer
	case "$answer" in
	y | Y | yes | Yes) ;;
	*)
		echo "Отменено."
		exit 1
		;;
	esac
fi

SNAPSHOT=$(mktemp -t splitr-pf-snapshot)
{
	echo "=== снимок состояния pf $(date) ==="
	echo "--- pfctl -s info ---"
	$PFCTL -s info 2>&1
	echo "--- pfctl -s Anchors ---"
	$PFCTL -s Anchors 2>&1
	echo "--- pfctl -s rules ---"
	$PFCTL -s rules 2>&1
	echo "--- pfctl -s nat ---"
	$PFCTL -s nat 2>&1
	echo "--- $PF_CONF ---"
	cat "$PF_CONF" 2>&1
} >"$SNAPSHOT" 2>&1
echo "Снимок состояния pf сохранён: $SNAPSHOT"

# Живой sshuttle/VPN сломался бы от перезагрузки главного набора правил.
LIVE_ANCHORS=$($PFCTL -s Anchors 2>/dev/null | tr -d ' ' | grep -E '^sshuttle-' || true)
if [[ -n "$LIVE_ANCHORS" ]]; then
	red "Активен туннель sshuttle (якоря: $(echo "$LIVE_ANCHORS" | tr '\n' ' '))."
	echo "Тест перезагружает главный набор правил pf и порвал бы туннель."
	echo "Опустите туннель (splitr down) и запустите снова."
	exit 1
fi

restored=0
restore() {
	[[ $restored -eq 1 ]] && return
	restored=1
	echo
	bold "Восстанавливаю pf"
	$PFCTL -a "$SSH_ANCHOR" -F all >/dev/null 2>&1
	$PFCTL -a "$E2E_ANCHOR" -F all >/dev/null 2>&1
	if $PFCTL -f "$PF_CONF" >/dev/null 2>&1; then
		green "  $PF_CONF перезагружен"
	else
		red "  НЕ УДАЛОСЬ перезагрузить $PF_CONF — выполните вручную: sudo pfctl -f $PF_CONF"
	fi
	# Тестовых якорей в главном наборе быть больше не должно.
	if $PFCTL -s rules 2>/dev/null | grep -q "$E2E_ANCHOR"; then
		red "  ВНИМАНИЕ: тестовые якоря всё ещё в главном наборе правил!"
		red "  Снимок исходного состояния: $SNAPSHOT"
	else
		green "  тестовые якоря сняты"
	fi
}
trap 'restore' EXIT
trap 'echo; red "прервано пользователем"; restore; exit 130' INT TERM

bold "1/3 Тесты разбора правил настоящим pfctl (без root)"
if ! run_as_user "$GO_BIN" test -count=1 "$REPO_ROOT/test/pfe2e/"; then
	red "Правила не разбираются pf — до e2e дело не дошло."
	exit 1
fi
green "  правила разбираются"
echo

bold "2/3 Сборка e2e-теста"
TESTBIN=$(mktemp -t splitr-pfe2e)
rm -f "$TESTBIN"
if ! run_as_user "$GO_BIN" test -tags pf_e2e -c -o "$TESTBIN" "$REPO_ROOT/test/pfe2e/"; then
	red "Не собрался тестовый бинарь."
	exit 1
fi
green "  собрано: $TESTBIN"
echo

bold "3/3 e2e на настоящем ядре (root)"
LOG=$(mktemp -t splitr-pfe2e-log)
"$TESTBIN" -test.v -test.count=1 -test.timeout "$TIMEOUT" -test.run TestProtectionOnRealKernel 2>&1 | tee "$LOG"
rc=${PIPESTATUS[0]}
rm -f "$TESTBIN"

# Пропущенный тест — это НЕ успех: ядро ничего не подтвердило.
skipped=0
if grep -q -- "--- SKIP" "$LOG" || grep -q "^--- SKIP" "$LOG"; then
	skipped=1
fi

restore
echo

# Финальная проверка: после отката сеть должна работать.
bold "Проверка сети после отката"
if nc -z -G 3 1.1.1.1 443 >/dev/null 2>&1 || nc -z -G 3 8.8.8.8 443 >/dev/null 2>&1; then
	green "  исходящие соединения работают"
else
	red "  исходящее соединение не установилось."
	echo "  Возможно, так и было до теста (нет интернета) — сверьтесь со снимком $SNAPSHOT."
	echo "  Если сеть пропала: sudo pfctl -f $PF_CONF, при необходимости sudo pfctl -d."
fi
echo

if [[ $rc -eq 0 && $skipped -eq 1 ]]; then
	yellow "ИТОГ: ТЕСТ ПРОПУЩЕН — ядро ничего не подтвердило."
	yellow "Причина напечатана выше (нет достижимой цели, поднят туннель, set skip on lo0)."
	yellow "Подробный вывод: $LOG"
	exit 2
fi

if [[ $rc -eq 0 ]]; then
	green "ИТОГ: ЗЕЛЁНО. Ядро подтверждает: блокировка без quick режет трафик,"
	green "живой якорь sshuttle её перебивает, а его исчезновение мгновенно"
	green "возвращает блокировку. panic режет даже при живом туннеле, lo0 не задет."
else
	red "ИТОГ: КРАСНО (код $rc). Смотрите вывод выше: там напечатаны и правила pf,"
	red "и состояние на момент провала. Исходное состояние pf: $SNAPSHOT"
	red "Полный вывод теста: $LOG"
fi
exit $rc
