#!/bin/sh
set -eu

umask 077
export HOME=/home/wechat
export DISPLAY="${WECHAT_DISPLAY:-:99}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/wechatcopilot/runtime/xdg}"
export LANG=C.UTF-8
export LC_ALL=C.UTF-8
export NO_AT_BRIDGE=0
export QT_ACCESSIBILITY=1
export QT_LINUX_ACCESSIBILITY_ALWAYS_ON=1
export QT_QPA_PLATFORM=xcb
export APPIMAGE_EXTRACT_AND_RUN=1

printf '%s' "$DBUS_SESSION_BUS_ADDRESS" > /wechatcopilot/runtime/dbus-address
chmod 0600 /wechatcopilot/runtime/dbus-address

dbus-update-activation-environment DISPLAY XDG_RUNTIME_DIR >/dev/null 2>&1 || true

if command -v at-spi-bus-launcher >/dev/null 2>&1; then
    at-spi-bus-launcher --launch-immediately >/dev/null 2>&1 &
elif [ -x /usr/libexec/at-spi-bus-launcher ]; then
    /usr/libexec/at-spi-bus-launcher --launch-immediately >/dev/null 2>&1 &
fi

openbox >/wechatcopilot/runtime/openbox.log 2>&1 &

if [ ! -x /opt/wechat/WeChat.AppImage ]; then
    echo "official WeChat AppImage is missing or not executable" >&2
    exit 1
fi

# The vendor client may emit account-derived diagnostics. Keep them out of the
# Docker log; status and screenshots are available through the control plane.
exec /opt/wechat/WeChat.AppImage >/dev/null 2>&1
