ARG BASE_IMAGE=ubuntu:24.04
FROM ${BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        at-spi2-core \
        ca-certificates \
        dbus-x11 \
        fonts-noto-cjk \
        imagemagick \
        libfuse2t64 \
        openbox \
        python3 \
        python3-pyatspi \
        python3-xlib \
        tesseract-ocr \
        tesseract-ocr-chi-sim \
        x11-utils \
        xclip \
        xvfb \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /opt/wechat /opt/wechatcopilot /home/wechat /wechatcopilot/runtime \
    && chmod 0755 /opt/wechat /opt/wechatcopilot \
    && chmod 0700 /home/wechat /wechatcopilot/runtime

RUN groupadd --gid 1000 wechat \
    && useradd --uid 1000 --gid 1000 --home-dir /home/wechat --shell /usr/sbin/nologin wechat \
    && chown -R 1000:1000 /home/wechat /wechatcopilot/runtime

COPY deploy/wechat/entrypoint.sh /opt/wechatcopilot/entrypoint
COPY deploy/wechat/session-entrypoint.sh /opt/wechatcopilot/session-entrypoint
COPY deploy/wechat/control.sh /opt/wechatcopilot/control
COPY deploy/wechat/screenshot.sh /opt/wechatcopilot/screenshot
COPY deploy/wechat/ui_driver.py /opt/wechatcopilot/ui_driver.py

RUN chmod 0755 \
        /opt/wechatcopilot/entrypoint \
        /opt/wechatcopilot/session-entrypoint \
        /opt/wechatcopilot/control \
        /opt/wechatcopilot/screenshot \
        /opt/wechatcopilot/ui_driver.py

WORKDIR /home/wechat
ENTRYPOINT ["/opt/wechatcopilot/entrypoint"]
