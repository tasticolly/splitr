#!/bin/sh
# Подготовка клиентского контейнера: ключ ssh с правильными правами,
# каталоги для стаба pfctl и логов. Демон запускают уже сами тесты.
set -eu

mkdir -p /etc/splitr /var/lib/pfstub/anchors /etc/pf.anchors /var/log
cp /keys/id_ed25519 /etc/splitr/id_ed25519
chmod 600 /etc/splitr/id_ed25519

echo "client: sshuttle $(sshuttle --version 2>&1 | head -1), pfctl -> стаб"
exec "$@"
