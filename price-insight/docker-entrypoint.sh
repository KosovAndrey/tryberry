#!/bin/sh
# Проверяем каталог стейта до старта JVM. Named volume наследует владельца из
# образа, а вот bind mount приедет root-овнершипом, и Streams упадёт где-то в
# середине инициализации с невнятной ошибкой. Лучше сказать прямо и сразу.
set -e

if [ ! -w "${STATE_DIR:-/var/lib/price-insight}" ]; then
    echo "FATAL: каталог стейта ${STATE_DIR:-/var/lib/price-insight} недоступен на запись." >&2
    echo "       Named volume наследует права из образа; для bind mount выставьте" >&2
    echo "       владельца вручную: chown -R 1000:1000 <путь>" >&2
    exit 1
fi

exec java $JAVA_OPTS -jar /app/app.jar "$@"
