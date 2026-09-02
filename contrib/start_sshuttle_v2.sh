#!/bin/zsh
# Замена старого ~/start_sshuttle_v2.sh на вызов splitr.
#
# Зачем менять: старый скрипт делал `sudo pfctl -F all`, а это вычищает главный
# набор правил pf вместе с вызовом якоря splitr — то есть незаметно снимает
# kill-switch. Демон это чинит, но только когда туннель опущен.
#
# Установка:
#   cp ~/projects/splitr/contrib/start_sshuttle_v2.sh ~/start_sshuttle_v2.sh

set -e

PROFILE="${1:-}"

if [ -z "$PROFILE" ]; then
  echo "Использование: $0 <профиль>"
  echo "Доступные профили:"
  splitr config show | awk '/^profiles:/{p=1;next} p && /^  [a-z]/{gsub(":","");print "  " $1}'
  exit 1
fi

splitr up "$PROFILE"
splitr status
