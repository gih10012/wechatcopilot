# WeChat Linux runtime

This image contains only the open-source desktop automation runtime. It never
contains or downloads WeChat. Build it from the repository root:

```sh
docker build \
  --file deploy/wechat/Dockerfile \
  --tag wechatcopilot/wechat-runtime:local \
  .
```

For a reproducible release, replace the default `BASE_IMAGE` with an Ubuntu
digest reviewed by the release workflow:

```sh
docker build \
  --build-arg 'BASE_IMAGE=ubuntu:24.04@sha256:<reviewed-digest>' \
  --file deploy/wechat/Dockerfile \
  --tag wechatcopilot/wechat-runtime:local \
  .
```

The Go driver starts the container with five explicit bind mounts:

- a checksum-pinned official AppImage at `/opt/wechat/WeChat.AppImage`, read-only;
- an account-specific synthetic home at `/home/wechat`;
- an account-specific received-file directory at `/home/wechat/WeChat_Files`;
- an account-specific runtime directory at `/wechatcopilot/runtime`;
- the account's stable synthetic machine ID at `/etc/machine-id`, read-only.

It deliberately does not mount the operator's home or `~/.xwechat`. The
container runs as the configured non-root UID/GID, drops Linux capabilities,
disables privilege escalation and does not publish a VNC, X11, D-Bus or control
port. Semantic operations enter through `docker exec` on stdin. At each reuse,
the configured image tag is resolved to an immutable local image ID and the
driver verifies the exact UID/GID, labels, environment, resource limits,
private mount propagation, five mounts, bridge network, capabilities, security
options, namespaces, devices, and absence of published ports. A legacy or
manually changed container is rejected instead of being reused.

The runtime combines Xvfb, Openbox, a private session D-Bus, AT-SPI2 and Qt's
accessibility bridge. `ui_driver.py` prefers accessible roles/actions and
returns a compatibility error when it cannot safely identify a target. XTest
is used only for semantic paste operations. Screenshots are returned directly
to the daemon and are not stored by the image.

The current source tree has no WCDB adapter; the driver therefore exposes only
an AT-SPI viewport path. It lists conversation rows currently rendered in the
left pane and messages
currently rendered in the selected conversation. Those records are explicitly
marked `source=ui`, `complete=false`, and with confidence below `1`. Sending
requires the exact title plus the freshly enumerated accessibility locator and
an exact, unique right-pane header, editor, and send button. Surface actions are
bound to a snapshot generation and semantic node signature. Duplicate titles,
off-screen rows, reused paths, changed pages, stale locators, unsupported unread
filters, and unknown message timestamps fail closed instead of being inferred.
