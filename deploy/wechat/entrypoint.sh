#!/bin/sh
set -eu

umask 077
export HOME=/home/wechat
export DISPLAY="${WECHAT_DISPLAY:-:99}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/wechatcopilot/runtime/xdg}"

mkdir -p "$HOME" "$XDG_RUNTIME_DIR" /wechatcopilot/runtime/outbox /tmp/.X11-unix
chmod 0700 "$HOME" "$XDG_RUNTIME_DIR" /wechatcopilot/runtime/outbox
chmod 1777 /tmp/.X11-unix

Xvfb "$DISPLAY" -screen 0 1440x960x24 -nolisten tcp -noreset &

attempt=0
while ! xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 100 ]; then
        echo "virtual X server did not become ready" >&2
        exit 1
    fi
    sleep 0.1
done

exec dbus-run-session -- /opt/wechatcopilot/session-entrypoint
