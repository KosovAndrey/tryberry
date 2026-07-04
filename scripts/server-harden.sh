#!/usr/bin/env bash
# Одноразовый хардинг прод-VPS (после инцидента 2026-07-03).
# Запуск на сервере: sudo ./scripts/server-harden.sh
#
# Что делает (идемпотентно, можно перезапускать):
#   1. UFW: default deny incoming; открыты только 22/80/443.
#   2. iptables DOCKER-USER: жёсткий DROP на published-порты контейнеров с
#      внешнего интерфейса, кроме 80/443. Docker обходит UFW — это единственный
#      надёжный заслон: даже если compose снова опубликует порт наружу
#      (регресс инцидента), снаружи он будет недоступен. Правила persistent.
#   3. SSH: только ключи (PasswordAuthentication no), root-логин запрещён.
#   4. fail2ban: бан брутфорса SSH.
#   5. unattended-upgrades: авто-патчи безопасности ОС.
#   6. chmod 600 .env.
#
# ВАЖНО до запуска: убедись, что заходишь на сервер ПО КЛЮЧУ (не по паролю),
# иначе шаг 3 отрежет доступ. Проверка: ssh -o PasswordAuthentication=no <host>.
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "нужен root: sudo $0"; exit 1; }

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
EXT_IF="$(ip route show default | awk '{print $5; exit}')"
[ -n "$EXT_IF" ] || { echo "не определил внешний интерфейс"; exit 1; }
echo "внешний интерфейс: $EXT_IF"

echo
echo "── 1/6 UFW ──────────────────────────────────────────────────────────────"
# ⚠️ НЕ ставить рядом iptables-persistent: он КОНФЛИКТУЕТ с ufw — apt при его
# установке молча удаляет ufw (наступили 2026-07-04: шаг 2 первой версии этого
# скрипта снёс ufw из шага 1). Персистентность DOCKER-USER — через after.rules
# самого ufw (шаг 2), сторонний механизм не нужен.
DEBIAN_FRONTEND=noninteractive apt-get purge -y -qq iptables-persistent netfilter-persistent >/dev/null 2>&1 || true
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ufw >/dev/null
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp

echo
echo "── 2/6 DOCKER-USER через ufw after.rules (Docker обходит UFW) ───────────"
# Docker публикует порты мимо UFW (цепочка DOCKER-USER в FORWARD). Схема:
#   ответный трафик (established) — пропустить;
#   новые соединения снаружи на 80/443 — пропустить (nginx);
#   всё остальное новое снаружи к контейнерам — DROP;
#   не с внешнего интерфейса (docker-сети между собой) — RETURN (не трогаем).
# Блок живёт в /etc/ufw/after.rules → ufw применяет его при каждом старте и
# reload — переживает ребут без iptables-persistent (см. конфликт выше).
AFTER_RULES=/etc/ufw/after.rules
# идемпотентно: вырезать старый блок, дописать свежий (EXT_IF мог смениться)
sed -i '/# BEGIN TRYBERRY DOCKER-USER/,/# END TRYBERRY DOCKER-USER/d' "$AFTER_RULES"
cat >> "$AFTER_RULES" <<EOF
# BEGIN TRYBERRY DOCKER-USER
*filter
:DOCKER-USER - [0:0]
-F DOCKER-USER
-A DOCKER-USER -i $EXT_IF -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN
-A DOCKER-USER -i $EXT_IF -p tcp --dport 80 -j RETURN
-A DOCKER-USER -i $EXT_IF -p tcp --dport 443 -j RETURN
-A DOCKER-USER -i $EXT_IF -j DROP
-A DOCKER-USER -j RETURN
COMMIT
# END TRYBERRY DOCKER-USER
EOF
ufw --force enable
ufw reload
ufw status verbose
iptables -L DOCKER-USER -n --line-numbers

echo
echo "── 3/6 SSH: только ключи, без root ──────────────────────────────────────"
mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/99-hardening.conf <<'EOF'
# Хардинг после инцидента 2026-07-03 (tryberrybot)
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
MaxAuthTries 4
X11Forwarding no
EOF
sshd -t   # валидация конфига ДО рестарта — не отрежь себе доступ
systemctl restart ssh 2>/dev/null || systemctl restart sshd
echo "sshd: пароли выключены, root запрещён (текущие сессии живут)"

echo
echo "── 4/6 fail2ban ─────────────────────────────────────────────────────────"
apt-get install -y -qq fail2ban >/dev/null
cat > /etc/fail2ban/jail.local <<'EOF'
[sshd]
enabled  = true
maxretry = 5
findtime = 10m
bantime  = 1h
# повторные залёты — бан по нарастающей до недели
bantime.increment = true
bantime.maxtime   = 1w
EOF
systemctl enable --now fail2ban
systemctl restart fail2ban
# демону нужна пара секунд на создание сокета — иначе status ложно пугает ошибкой
for i in 1 2 3 4 5; do sleep 1; fail2ban-client ping >/dev/null 2>&1 && break; done
fail2ban-client status sshd || true

echo
echo "── 5/6 unattended-upgrades (авто-патчи безопасности) ────────────────────"
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq unattended-upgrades >/dev/null
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
systemctl enable --now unattended-upgrades 2>/dev/null || true
echo "unattended-upgrades: включён"

echo
echo "── 6/6 права на секреты ─────────────────────────────────────────────────"
if [ -f "$PROJECT_DIR/.env" ]; then
  chmod 600 "$PROJECT_DIR/.env"
  ls -l "$PROJECT_DIR/.env"
else
  echo "внимание: $PROJECT_DIR/.env не найден — проверь вручную"
fi

echo
echo "══ ГОТОВО ══"
echo "Проверь с ДРУГОЙ машины: nmap -Pn <IP> → открыты только 22/80/443."
echo "И НЕ закрывая эту сессию — что новый ssh по ключу заходит."
