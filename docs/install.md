# Installation

The source tree builds a Linux CLI locally. The future release workflow targets Linux amd64 and arm64 binaries; no stable release or real-account compatibility claim exists yet. The WeCom/Redroid compatibility target is Linux amd64, and driver capability output remains authoritative on other hosts. Build and installation do not download or redistribute Tencent clients. Operators obtain the current official WeChat AppImage and WeCom APK directly from Tencent during driver setup.

## Prerequisites

- Linux with systemd user services and at least 30 GB free on the state filesystem.
- Go 1.26 or newer for source builds.
- Docker Engine with a working non-root client connection.
- For WeChat: Xvfb, a lightweight X11 window manager, D-Bus, AT-SPI2, XTest, clipboard/XDND tooling, and OCR data for the languages in use.
- For WeCom: Binder/BinderFS kernel support, Docker privileges appropriate for Redroid, and a JDK compatible with the included Gradle wrapper when building the companion.

Do not grant broad Docker access without understanding that membership in the Docker group is effectively root-equivalent. A rootless-compatible runtime is desirable but is not the v0.1 compatibility target.

## Build from source

```bash
git clone https://github.com/gih10012/wechatcopilot.git
cd wechatcopilot
go build -trimpath -o bin/wechatcopilot ./cmd/wechatcopilot
go test ./...
```

Build a debug-signed companion for local development with `make companion`; it produces `android/companion/build/outputs/apk/debug/companion-debug.apk`. Production profiles should use `wechatcopilot-companion.apk` from a verified GitHub Release, or a release APK signed with an operator-controlled dedicated key. Never substitute the official WeCom APK for the companion path.

Run the preflight before adding an account:

```bash
./bin/wechatcopilot doctor --json
```

`doctor` checks state-disk capacity and semantics, exact runtime-directory and Unix-socket ownership/modes, Docker connectivity, locally installed runtime images, Binder support, and official-client artifact structure and hashes. Image checks use `docker image inspect` and never pull. Failed JSON checks include a `fix` field when a bounded operator action is known; `doctor` does not run privileged changes.

## Build the WeChat runtime

The runtime image contains only project automation and Linux distribution packages. Build it locally; the AppImage is mounted later and never copied into the image:

```bash
docker build \
  --build-arg BASE_IMAGE='ubuntu:24.04@sha256:REVIEWED_DIGEST' \
  -f deploy/wechat/Dockerfile \
  -t wechatcopilot/wechat-runtime:local \
  .
```

Resolve and review the current Ubuntu digest before replacing `REVIEWED_DIGEST`. For a local throwaway build, the Dockerfile's unpinned default is available, but release and unattended deployments should always pin the base image.

Download the official AppImage from an approved Tencent HTTPS host, verify its SHA-256 independently, mark it executable, and set `WECHATCOPILOT_WECHAT_APPIMAGE` to its absolute path and `WECHATCOPILOT_WECHAT_APPIMAGE_SHA256` to the exact 64-character digest. Set `WECHATCOPILOT_WECHAT_IMAGE` to the local runtime image tag. Startup resolves that tag to its immutable local image ID and creates the container from the ID, so retagging the name later cannot silently change an existing profile runtime. The client file must be a non-empty executable ELF regular file and cannot be a symlink. Both `doctor` and driver startup independently verify the configured digest and fail closed before the file is mounted read-only.

An existing WeChat container is reused only when its immutable image ID, image-derived configuration, account labels, UID/GID, environment, memory and shared-memory limits, private bind mounts, network, capabilities, namespace settings, and zero published ports all match. Containers created by an older or manually changed layout fail with `CLIENT_INCOMPATIBLE`. Review the named container's ownership labels, stop it, and remove only that container before activating the account again; its bind-mounted persistent profile remains the source of login state. `accounts remove --purge` is a different destructive operation that also deletes the saved profile.

For WeCom, configure a locally available Redroid image pinned by digest, an official APK URL and independently verified SHA-256, and the project-built companion APK. Control is isolated behind Docker exec and no companion port is published. `doctor` verifies the pinned image locally, both APK structures, the official APK digest, and that the two APK paths resolve to different files. Start with a disposable compatibility profile and stop if the official client rejects the environment or reports account risk.

The daemon reads driver configuration from its environment. `wechatcopilot daemon install` supports a private environment file at `${XDG_CONFIG_HOME:-$HOME/.config}/wechatcopilot/environment`. Keep it mode `0600`; it may contain paths and digests but must never contain login codes or account credentials.

| Variable | Meaning |
| --- | --- |
| `WECHATCOPILOT_WECHAT_IMAGE` | Local project runtime image tag. |
| `WECHATCOPILOT_WECHAT_APPIMAGE` | Absolute verified official AppImage path. |
| `WECHATCOPILOT_WECHAT_APPIMAGE_SHA256` | Independently obtained 64-character AppImage digest. |
| `WECHATCOPILOT_LAN_ADDRESS` | Optional RFC1918 address used only when login is explicitly started with `--lan`. It must be assigned to an eligible local interface. |
| `WECHATCOPILOT_WECOM_REDROID_IMAGE` | Redroid image reference pinned with `@sha256:`. |
| `WECHATCOPILOT_WECOM_APK_URL` | Approved official Tencent HTTPS APK URL. |
| `WECHATCOPILOT_WECOM_APK_SHA256` | Independently obtained 64-character APK digest. |
| `WECHATCOPILOT_WECOM_APK` | Absolute destination/existing official APK path. |
| `WECHATCOPILOT_WECOM_COMPANION_APK` | Absolute signed project companion APK path. |

## State location

The default persistent state is `${XDG_STATE_HOME:-$HOME/.local/state}/wechatcopilot`. Override it before first use when a larger encrypted disk is available:

```bash
export WECHATCOPILOT_HOME=/srv/private/wechatcopilot-state
```

The chosen filesystem must support Unix ownership/modes, advisory locking, and SQLite WAL. Do not place live state on FAT, SMB, object-backed FUSE, or a synchronization folder. Back up the directory only while the corresponding account is stopped or with a filesystem snapshot.

## Start the daemon

For foreground development:

```bash
./bin/wechatcopilot daemon serve
```

For unattended use, install the project-supplied systemd user unit:

```bash
wechatcopilot daemon install
```

The command refuses to replace an existing unit unless `--force` is explicit and starts it through `systemctl --user`. Use `--no-start` to inspect the generated unit before enabling it. An administrator may run `loginctl enable-linger USERNAME` for host-boot startup; `wechatcopilot` never elevates privileges. The daemon socket remains accessible only to the operator UID.

## Add and log in to an account

```bash
wechatcopilot accounts add --platform wechat --alias personal --json
wechatcopilot accounts activate --account ACCOUNT_ID --json
wechatcopilot accounts login --account ACCOUNT_ID --lan --lan-address 192.168.1.20 --json
```

Omit `--lan` when the browser is on the same host. With `--lan`, omit `--lan-address` to prefer the default-route LAN interface, or provide an exact RFC1918 address already assigned to an eligible local interface. The command rejects public, wildcard, loopback, container-bridge, and unassigned addresses. It prints a one-time URL and expiry. Complete QR scan, phone confirmation, or verification-code entry directly on that page. A successful page remains available for about 60 seconds to display completion, then the login listener closes.

Repeat with `--platform wecom` for a WeCom profile after the Redroid compatibility check passes. Each account is newly authenticated in its own isolated profile; the installer does not read, stop, or copy an existing desktop login.

## Verify persistence

After an account reports `ONLINE`, restart its runtime and then the daemon. Confirm status after each restart:

```bash
wechatcopilot accounts status --account ACCOUNT_ID --json
```

A saved profile normally survives client, container, daemon, and host restarts on the same machine. Tencent can invalidate a session at any time; the correct response is a new user-completed login challenge, not an authentication bypass.

## Multiple accounts

Profiles are saved independently. Activating another account for the same platform stops and flushes the current one before starting the requested profile. Inactive accounts remain indexed but are not kept current. One WeChat account and one WeCom account may be active concurrently.

## MCP and Skill

Expose the stdio MCP server to a local agent with:

```bash
wechatcopilot mcp serve
```

Install `skills/wechatcopilot` through the supported Skill mechanism of the agent host. The Skill contains operating policy and uses the same daemon; it does not embed credentials or a second implementation.
