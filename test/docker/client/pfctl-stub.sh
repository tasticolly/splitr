#!/bin/sh
# Стаб pfctl(8) для e2e-стенда splitr.
#
# В Linux-контейнере пакетного фильтра pf не существует, поэтому подменяется
# сама утилита: стаб честно эмулирует то подмножество команд macOS pfctl,
# которым пользуется splitr, и хранит состояние в файлах.
#
# Что эмулируется:
#   -E                     включить pf, напечатать в stderr "Token : <N>"
#   -X <token>             отпустить ссылку на включённый pf
#   -s info                "Status: Enabled|Disabled"
#   -s rules               главный набор правил
#   -s Anchors             список якорей; sshuttle-<port> появляется ровно тогда,
#                          когда в системе реально живёт процесс sshuttle (pgrep)
#   -a <anchor> -f -       загрузить правила якоря со stdin
#   -a <anchor> -F all     очистить якорь
#   -a <anchor> -s rules   показать правила якоря
#   -F all                 очистить всё (эмуляция чужого сброса)
#   -f <file>              перечитать главный набор из файла
#   -k <a> -k <b>          сбросить состояния (только запись в лог)
#
# Все вызовы пишутся в $PFSTUB_DIR/calls.log — тесты проверяют по нему
# последовательность обращений демона к pf.
set -eu

DIR="${PFSTUB_DIR:-/var/lib/pfstub}"
ANCHORS="$DIR/anchors"
MAIN="$DIR/main.rules"
LOG="$DIR/calls.log"
ENABLED="$DIR/enabled"
TOKENS="$DIR/tokens"
# Порт, который стаб приписывает якорю sshuttle. В macOS его выбирает сам
# sshuttle; для стенда важно только имя вида sshuttle-<port>.
SSHUTTLE_PORT="${PFSTUB_SSHUTTLE_PORT:-12300}"

mkdir -p "$ANCHORS"
[ -f "$MAIN" ] || : > "$MAIN"
[ -f "$ENABLED" ] || echo 0 > "$ENABLED"
[ -f "$TOKENS" ] || : > "$TOKENS"

# --- разбор аргументов -------------------------------------------------------
anchor=""; op=""; what=""; file=""; token=""; kill_args=""
argv="$*"
while [ $# -gt 0 ]; do
	case "$1" in
	-a) anchor="${2:-}"; shift 2 ;;
	-E) op="enable"; shift ;;
	-X) op="release"; token="${2:-}"; shift 2 ;;
	-s) op="show"; what="${2:-}"; shift 2 ;;
	-f) op="load"; file="${2:-}"; shift 2 ;;
	-F) op="flush"; what="${2:-}"; shift 2 ;;
	-k) op="killstates"; kill_args="$kill_args ${2:-}"; shift 2 ;;
	-q | -v | -e | -d) shift ;;
	*) shift ;;
	esac
done

stdin_note=""
if [ "$op" = "load" ] && [ "$file" = "-" ]; then
	stdin_data="$(cat)"
	stdin_note=" <stdin:$(printf '%s' "$stdin_data" | wc -c | tr -d ' ')b>"
fi

printf '%s\tpfctl %s%s\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "$argv" "$stdin_note" >> "$LOG"

anchor_file() { printf '%s/%s.rules' "$ANCHORS" "$(printf '%s' "$1" | tr '/' '_')"; }

sshuttle_alive() {
	# Ключевой сигнал состояния для демона: якорь sshuttle в pf существует
	# ровно пока жив процесс sshuttle.
	pgrep -f 'sshuttle' > /dev/null 2>&1
}

case "$op" in
enable)
	echo 1 > "$ENABLED"
	n=$(( $(wc -l < "$TOKENS") + 1 ))
	tok=$(( 10000 + n ))
	echo "$tok" >> "$TOKENS"
	echo "pf enabled" >&2
	echo "Token : $tok" >&2
	;;

release)
	if [ -n "$token" ]; then
		grep -v -x "$token" "$TOKENS" > "$TOKENS.tmp" 2>/dev/null || : > "$TOKENS.tmp"
		mv "$TOKENS.tmp" "$TOKENS"
	fi
	[ -s "$TOKENS" ] || echo 0 > "$ENABLED"
	;;

show)
	case "$what" in
	info)
		if [ "$(cat "$ENABLED")" = "1" ]; then
			echo "Status: Enabled for 0 days 00:00:01           Debug: Urgent"
		else
			echo "Status: Disabled                              Debug: Urgent"
		fi
		echo ""
		echo "State Table                          Total             Rate"
		;;
	rules)
		if [ -n "$anchor" ]; then
			f="$(anchor_file "$anchor")"
			[ -f "$f" ] && cat "$f" || true
		else
			cat "$MAIN"
		fi
		;;
	Anchors)
		# Якоря главного набора.
		sed -n 's/^[[:space:]]*anchor[[:space:]]*"\([^"*]*\)".*$/\1/p' "$MAIN" || true
		# Якорь sshuttle существует, только пока жив процесс sshuttle.
		if sshuttle_alive; then
			echo "sshuttle-$SSHUTTLE_PORT"
		fi
		;;
	States | Tables | nat | all)
		;;
	*)
		echo "pfctl: неизвестный параметр -s $what" >&2
		exit 1
		;;
	esac
	;;

load)
	if [ -n "$anchor" ]; then
		[ "$file" = "-" ] || stdin_data="$(cat "$file")"
		printf '%s\n' "$stdin_data" > "$(anchor_file "$anchor")"
	else
		# Перезагрузка главного набора из файла: как настоящий pfctl -f,
		# сносит все динамически добавленные якоря (включая sshuttle).
		[ -f "$file" ] || { echo "pfctl: $file: No such file or directory" >&2; exit 1; }
		grep -E '^[[:space:]]*(anchor|load anchor|scrub-anchor|nat-anchor|rdr-anchor|dummynet-anchor)' "$file" > "$MAIN" || : > "$MAIN"
		# Директивы load anchor "X" from "path" подтягивают файлы якорей с диска.
		sed -n 's/^[[:space:]]*load anchor[[:space:]]*"\([^"]*\)"[[:space:]]*from[[:space:]]*"\([^"]*\)".*$/\1\t\2/p' "$file" |
			while IFS="$(printf '\t')" read -r name path; do
				if [ -f "$path" ]; then
					cp "$path" "$(anchor_file "$name")"
				fi
			done
	fi
	;;

flush)
	case "$what" in
	all | rules | states)
		if [ -n "$anchor" ]; then
			: > "$(anchor_file "$anchor")"
		else
			# Чужой `pfctl -F all`: сносит и главный набор, и все якоря.
			: > "$MAIN"
			rm -f "$ANCHORS"/*.rules 2>/dev/null || true
		fi
		echo "rules cleared" >&2
		;;
	*)
		echo "pfctl: неизвестный параметр -F $what" >&2
		exit 1
		;;
	esac
	;;

killstates)
	echo "killed 0 states from 1 sources and 1 destinations" >&2
	;;

"")
	echo "pfctl: стаб вызван без поддерживаемых аргументов: $argv" >&2
	exit 1
	;;
esac

exit 0
