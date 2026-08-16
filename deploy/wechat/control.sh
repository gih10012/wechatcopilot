#!/bin/sh
set -eu

export HOME=/home/wechat
export DISPLAY="${WECHAT_DISPLAY:-:99}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/wechatcopilot/runtime/xdg}"
export NO_AT_BRIDGE=0

address_file=/wechatcopilot/runtime/dbus-address
deadline=$(( $(date +%s) + 10 ))
while :; do
    if [ -r "$address_file" ]; then
        DBUS_SESSION_BUS_ADDRESS=$(sed -n '1p' "$address_file")
        export DBUS_SESSION_BUS_ADDRESS
        if dbus-send --session --reply-timeout=250 --dest=org.a11y.Bus --type=method_call --print-reply \
            /org/a11y/bus org.a11y.Bus.GetAddress >/dev/null 2>&1; then
            break
        fi
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
        printf '%s\n' '{"ok":false,"code":"CLIENT_INCOMPATIBLE","error":"accessibility session is not ready"}'
        exit 0
    fi
    sleep 0.1
done

exec python3 /opt/wechatcopilot/ui_driver.py
