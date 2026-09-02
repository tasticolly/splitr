#!/usr/bin/env bash
# Полный прогон e2e-стенда splitr: поднять, прогнать тесты, всё убрать.
# Никакого sudo и никаких артефактов в репозитории: ключи ssh генерируются
# во временный каталог вне дерева проекта.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE=(docker compose -f "$HERE/docker-compose.yml")

SPLITR_E2E_KEYDIR="$(mktemp -d "${TMPDIR:-/tmp}/splitr-e2e-keys.XXXXXX")"
export SPLITR_E2E_KEYDIR

KEEP="${SPLITR_E2E_KEEP:-0}"
rc=1

cleanup() {
	rc=$?
	set +e
	if [ "$rc" -ne 0 ]; then
		echo
		echo "=== логи контейнеров (прогон упал) ==============================="
		"${COMPOSE[@]}" logs --no-color --tail 80
		echo "=== лог демона ==================================================="
		"${COMPOSE[@]}" exec -T client sh -c 'tail -80 /var/log/splitr.log' 2>/dev/null
		echo "=== лог вызовов стаба pfctl ======================================"
		"${COMPOSE[@]}" exec -T client sh -c 'tail -40 /var/lib/pfstub/calls.log' 2>/dev/null
		echo "=================================================================="
	fi
	if [ "$KEEP" = "1" ]; then
		echo "SPLITR_E2E_KEEP=1 — стенд оставлен поднятым, ключи в $SPLITR_E2E_KEYDIR"
	else
		"${COMPOSE[@]}" down -v --remove-orphans --timeout 5 >/dev/null 2>&1
		rm -rf "$SPLITR_E2E_KEYDIR"
	fi
	exit "$rc"
}
trap cleanup EXIT

echo "==> генерирую одноразовый ключ ssh"
ssh-keygen -t ed25519 -N '' -C splitr-e2e -f "$SPLITR_E2E_KEYDIR/id_ed25519" -q
chmod 600 "$SPLITR_E2E_KEYDIR/id_ed25519"

echo "==> убираю следы прошлого прогона"
"${COMPOSE[@]}" down -v --remove-orphans --timeout 5 >/dev/null 2>&1 || true

echo "==> собираю образы"
"${COMPOSE[@]}" build --quiet

echo "==> поднимаю стенд"
"${COMPOSE[@]}" up -d --force-recreate

echo "==> жду sshd на remote"
for i in $(seq 1 60); do
	if "${COMPOSE[@]}" exec -T client sh -c \
		'ssh -i /etc/splitr/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 root@10.77.1.20 true' >/dev/null 2>&1; then
		echo "    ssh до remote готов (попытка $i)"
		break
	fi
	if [ "$i" = 60 ]; then
		echo "remote так и не принял ssh" >&2
		exit 1
	fi
	sleep 1
done

echo "==> прогоняю e2e-тесты"
"${COMPOSE[@]}" exec -T \
	-e SPLITR_E2E=1 \
	client go test -tags docker_e2e -count=1 -timeout 20m -v ./test/e2e/...
