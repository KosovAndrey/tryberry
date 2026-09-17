#!/bin/sh
# Xvfb → Chromium работает headful, затем сам сервис.
set -e
export DISPLAY="${DISPLAY:-:99}"
rm -f /tmp/.X11-unix/X"${DISPLAY#:}" /tmp/.X"${DISPLAY#:}"-lock 2>/dev/null || true
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/xvfb.log 2>&1 &
sleep 2
exec python watch.py "$@"
