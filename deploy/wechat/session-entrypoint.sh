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
else
    echo "AT-SPI bus launcher is unavailable" >&2
    exit 1
fi

deadline=$(( $(date +%s) + 10 ))
while ! dbus-send --session --reply-timeout=250 --dest=org.a11y.Bus --type=method_call --print-reply \
    /org/a11y/bus org.a11y.Bus.GetAddress >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
        echo "AT-SPI accessibility bus did not become ready" >&2
        exit 1
    fi
    sleep 0.1
done

openbox >/wechatcopilot/runtime/openbox.log 2>&1 &

if [ ! -x /opt/wechat/WeChat.AppImage ]; then
    echo "official WeChat AppImage is missing or not executable" >&2
    exit 1
fi

# The vendor client may emit account-derived diagnostics. Keep them out of the
# Docker log; status and screenshots are available through the control plane.
exec /opt/wechat/WeChat.AppImage >/dev/null 2>&1
