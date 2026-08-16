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
| `WECHATCOPILOT_STATE_MOUNT_SOURCE` | Optional exact block-device path required for the state root, such as `/dev/mapper/wechatcopilot-state`. |
| `WECHATCOPILOT_STATE_MOUNT_FSTYPE` | Required filesystem type when a state mount source is configured. |
| `WECHATCOPILOT_STATE_MOUNT_UUID` | Canonical filesystem UUID required when a state mount source is configured. |
| `WECHATCOPILOT_STRICT_SWAP` | Optional boolean. When `true`, block daemon startup if swap is not zram-without-writeback or dm-crypt; default `false` reports a warning without changing host swap behavior. `daemon install` pins the normalized value in a required service environment file. |
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

### File-backed encrypted state volume

When only an NTFS3 data disk has enough capacity, use it only as the outer filesystem that stores the encrypted image. The default workflow fully allocates a 64 GiB file, formats that file as LUKS2, and formats the decrypted mapper as ext4. `WECHATCOPILOT_HOME` points to the inner ext4 mount, never to NTFS3. This is less robust than a dedicated native Linux partition: damage, truncation, Windows hibernation, or exhaustion of the outer filesystem can damage the entire inner volume. Disable Windows Fast Startup, keep an offline backup, and never copy the backing file while the inner filesystem is mounted.

The included provisioning script refuses sparse allocation, existing images, symlink targets, finite `/share` automount idle timeouts, unexpected key files, wrong backing-filesystem UUIDs, and colliding mappings. It never accepts a passphrase option; `cryptsetup` reads the passphrase directly from the operator's terminal.

First remove a finite `x-systemd.idle-timeout` from the `/share` entry in `/etc/fstab`, or set it to `0`/`0s`, then reload the system manager. The script checks both `/etc/fstab` and the effective automount unit and rejects a stale finite timeout:

```bash
sudo systemctl daemon-reload
```

Use the UUID of the filesystem mounted at `/share` as `OUTER_FILESYSTEM_UUID`. This is the outer backing-filesystem UUID, not the LUKS UUID and not the inner ext4 UUID. Pass it to every state-changing command (`create`, `configure`, `unlock`, and `lock`). The default image is 64 GiB; if `create` uses a non-default `--size-gib`, pass that same value to later `configure`, `unlock`, and `lock` operations so they can detect truncation. Every operation also rejects a sparse or deallocated backing file. Inspect the plan first:

```bash
./scripts/provision_state_volume.sh preflight \
  --backing-fs-uuid OUTER_FILESYSTEM_UUID
```

Create the fully allocated 64 GiB LUKS2 image and inner ext4 filesystem only from a trusted interactive terminal:

```bash
sudo ./scripts/provision_state_volume.sh create \
  --backing-fs-uuid OUTER_FILESYSTEM_UUID \
  --owner "$USER" \
  --confirm-create
```

`create` first asks `cryptsetup` to set and confirm a recovery passphrase, then asks for that same passphrase once more to open the new LUKS2 image. Password input is not echoed. It formats and verifies ext4, writes the volume marker, and installs marker-delimited `noauto` entries in `/etc/crypttab` and `/etc/fstab`. It then deliberately unmounts and closes the manually opened mapper. Systemd reopens and mounts it to verify the persistent path, so expect one final request for the same passphrase before `create` succeeds. The workflow does not install a key file, enroll a TPM, or enable automatic unlock.

On success the inner ext4 filesystem is mounted at `/srv/wechatcopilot-state`; the directory below an unmounted volume remains `root:root 0700`. Copy the four non-secret assignments printed by the script into the current shell. The first selects the state home; the other three are an all-or-nothing mount gate and use the inner ext4 device and filesystem UUID:

```bash
export WECHATCOPILOT_HOME=/srv/wechatcopilot-state
export WECHATCOPILOT_STATE_MOUNT_SOURCE=/dev/mapper/wechatcopilot-state
export WECHATCOPILOT_STATE_MOUNT_FSTYPE=ext4
export WECHATCOPILOT_STATE_MOUNT_UUID=INNER_EXT4_FILESYSTEM_UUID
```

Use the exact printed inner UUID, not the outer `/share` UUID. With all three gate variables exported, `doctor`, `daemon serve`, and `daemon install` validate the exact filesystem-root mount, kernel mount ID, `rw,nosuid,nodev` options, filesystem type, device major/minor, and UUID before creating state directories. A locked, bind-mounted, or wrong volume therefore cannot produce an unencrypted fallback profile.

`daemon install` reads the three already-exported gate variables and writes their normalized values to `${XDG_CONFIG_HOME:-$HOME/.config}/wechatcopilot/state-mount.environment` with mode `0600`. The generated user unit treats that file as required and loads it after the optional general `environment` file, so general driver settings cannot override the gate. The file and its generated unit reference remain downgrade markers: `doctor`, foreground `daemon serve`, and later installs refuse to proceed when a new shell omits the three constraints. Always export `WECHATCOPILOT_HOME` and all three gate variables before the initial install or any `daemon install --force`, and export them in every shell that runs `doctor` or `daemon serve`.

`create` already installs and verifies the system files. Use `configure` to reinstall or revalidate those entries for an existing volume. It also provides the safe resume path if `create` reports that the completed encrypted image was retained after the staging file had already become the final `state.luks`: do not rerun `create` and do not delete that final image. Run `configure` with the same outer UUID, owner, paths, mapper name, and non-default size (if any). For a locked image it requests the existing passphrase, mounts it, and validates the LUKS mapping, ext4 filesystem, and volume marker before reporting success:

```bash
sudo ./scripts/provision_state_volume.sh configure \
  --backing-fs-uuid OUTER_FILESYSTEM_UUID \
  --owner "$USER"
```

After a reboot, unlock from a trusted terminal before starting the user daemon:

```bash
sudo ./scripts/provision_state_volume.sh unlock \
  --backing-fs-uuid OUTER_FILESYSTEM_UUID \
  --owner "$USER"
```

Before locking, stop the daemon and every WeChat/WeCom container. The script verifies that the user service is not active and that no container with either project driver label is running; `--confirm-daemon-stopped` is an acknowledgement, not a bypass:

```bash
wechatcopilot daemon stop
docker ps --filter label=io.wechatcopilot.driver
docker ps --filter label=dev.wechatcopilot.driver
```

Only after both are stopped, run:

```bash
sudo ./scripts/provision_state_volume.sh lock \
  --backing-fs-uuid OUTER_FILESYSTEM_UUID \
  --owner "$USER" \
  --confirm-daemon-stopped
```

The supported workflow intentionally requires a manually entered passphrase for every unlock. The script does not configure TPM enrollment or any automatic unlock, and refuses the conventional auto-discovered key-file locations for this mapper. Keep a recovery passphrase and an offline LUKS header backup; never store a key beside the backing file or in the daemon environment.

Raw or unencrypted disk-backed swap can retain decrypted messages, screenshots, and keys even when the state filesystem uses LUKS. It can also be important for memory-pressure behavior and hibernation, so WeChat Copilot never disables or reconfigures host swap. The provisioning script reports non-zram swap, and `doctor` returns a non-blocking `swap_confidentiality` warning by default. Set `WECHATCOPILOT_STRICT_SWAP=true` only when the operator explicitly wants `doctor`, daemon startup, and account restoration to fail unless every active target is a real zram block device whose sysfs `backing_dev` is absent or `none`, or a separately reviewed dm-crypt swap device. `doctor` reads the current shell value; `daemon install` writes the normalized value to the required mode-`0600` `swap-policy.environment` file, loaded after the optional general environment file, so rerun installation with `--force` to change the daemon policy. Zram configured with disk writeback does not satisfy strict mode. Hibernation requires its own recoverable encrypted-swap design.

## Start the daemon

For foreground development:

```bash
./bin/wechatcopilot daemon serve
```

For unattended use, install the project-supplied systemd user unit:

```bash
wechatcopilot daemon install
```

The command refuses to replace an existing unit unless `--force` is explicit. A normal install enables and restarts the service so the newly written unit and mount gate apply even when an older daemon is already active. Use `--no-start` to enable without starting or restarting it while you inspect the generated unit. When the encrypted mount gate is enabled, run this command only after unlocking the volume and exporting `WECHATCOPILOT_HOME` plus all three gate variables; the required `state-mount.environment` file is then generated as described above. An administrator may run `loginctl enable-linger USERNAME` for host-boot startup, but the encrypted state volume still requires its separate manual unlock; `wechatcopilot` never elevates privileges or configures TPM unlock. The daemon socket remains accessible only to the operator UID.

## Add and log in to an account

```bash
wechatcopilot accounts add --platform wechat --alias personal --json
wechatcopilot accounts activate --account ACCOUNT_ID --json
```

Run the following command yourself in a separate trusted terminal. Do not have
an agent execute it or paste its output into an agent conversation; the output
contains a one-time bearer URL:

```bash
wechatcopilot accounts login --account ACCOUNT_ID --lan --lan-address 192.168.1.20 --json
```

Omit `--lan` when the browser is on the same host. With `--lan`, omit `--lan-address` to prefer the default-route LAN interface, or provide an exact RFC1918 address already assigned to an eligible local interface. The command rejects public, wildcard, loopback, container-bridge, and unassigned addresses. It prints a one-time URL and expiry. Complete QR scan, phone confirmation, or verification-code entry directly on that page. A successful page remains available for about 60 seconds to display completion, then the login listener closes.

Repeat with `--platform wecom` for a WeCom profile after the Redroid compatibility check passes. Each account is newly authenticated in its own isolated profile; the installer does not read, stop, or copy an existing desktop login.

## Verify persistence

After an account reports `ONLINE`, restart its runtime and then the daemon. Confirm status after each restart:

```bash
wechatcopilot accounts status --account ACCOUNT_ID --json
```

A saved profile normally survives client, container, daemon, and host restarts on the same machine. A transient driver dependency failure during daemon startup leaves the requested account active with a degraded status; restore the dependency and explicitly activate that same account, or restart the daemon, to retry without losing the requested slot. Tencent can invalidate a session at any time; the correct response is a new user-completed login challenge, not an authentication bypass.

## Multiple accounts

Profiles are saved independently. Activating another account for the same platform stops and flushes the current one before starting the requested profile. Inactive accounts remain indexed but are not kept current. One WeChat account and one WeCom account may be active concurrently.

## MCP and Skill

Expose the stdio MCP server to a local agent with:

```bash
wechatcopilot mcp serve
```

Install `skills/wechatcopilot` through the supported Skill mechanism of the agent host. The directory is included in every Linux release archive and in a source checkout. The Skill contains operating policy and uses the same daemon; it does not embed credentials or a second implementation.
