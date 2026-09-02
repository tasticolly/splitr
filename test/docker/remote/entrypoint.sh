#!/bin/sh
# Кладём публичный ключ стенда и поднимаем sshd в форграунде.
set -eu

mkdir -p /root/.ssh
cp /keys/id_ed25519.pub /root/.ssh/authorized_keys
chmod 700 /root/.ssh
chmod 600 /root/.ssh/authorized_keys

echo "remote: sshd стартует, python3 $(python3 --version 2>&1)"
exec /usr/sbin/sshd -D -e
