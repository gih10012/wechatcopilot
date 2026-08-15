#!/bin/sh
set -eu

export DISPLAY="${WECHAT_DISPLAY:-:99}"
exec import -silent -display "$DISPLAY" -window root png:-
