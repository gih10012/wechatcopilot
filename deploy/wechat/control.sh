#!/bin/sh
set -eu

export HOME=/home/wechat
export DISPLAY="${WECHAT_DISPLAY:-:99}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/wechatcopilot/runtime/xdg}"
export NO_AT_BRIDGE=0

address_file=/wechatcopilot/runtime/dbus-address
if [ ! -r "$address_file" ]; then
    printf '%s\n' '{"ok":false,"code":"CLIENT_INCOMPATIBLE","error":"accessibility session is not ready"}'
    exit 0
fi
DBUS_SESSION_BUS_ADDRESS=$(sed -n '1p' "$address_file")
export DBUS_SESSION_BUS_ADDRESS

exec python3 /opt/wechatcopilot/ui_driver.py
