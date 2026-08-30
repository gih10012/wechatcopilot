# WeCom experimental runtime

The daemon creates one persistent Redroid container per saved WeCom account.
`compose.yaml` is a review and diagnostics template; normal operation uses the
Go runtime in `internal/driver/wecom` so activation can be serialized.

## Inputs

- A Redroid Android 13 image pinned by immutable `@sha256` digest.
- The unmodified official WeCom APK downloaded from an allowlisted Tencent
  HTTPS host and pinned by an independently recorded SHA-256.
- `android/companion` built from this repository. It has no third-party
  runtime network, telemetry, Frida, TLS interception, or shell endpoint.
- An absolute, mode `0700` account directory on a filesystem with ordinary
  Linux locking and persistence semantics.

Neither client APK nor the Redroid image is redistributed by this project.
The runtime uses Docker's `--pull=never`; operators must fetch and review the
pinned image explicitly before activation.
No ADB, companion, or debug port is published on the host. The daemon creates
one non-attachable bridge network per account so the official client retains
normal outbound connectivity without sharing the default Docker bridge.
After Android boot, activation disables `adbd` TCP persistence, stops `adbd`,
and verifies that neither `/proc/net/tcp` table has a listener on port 5555;
failure is a compatibility error and the container is stopped.

The chosen Redroid image must natively support every ABI advertised by the
official APK. `INSTALL_FAILED_NO_MATCHING_ABIS`, an emulator/risk warning, or
any request for anti-detection changes is a failed feasibility gate: the
driver stops and does not download a native bridge or attempt a bypass.

## Persistence and authentication

The entire Redroid `/data` tree belongs to one local account ID. Deactivation
stops its ownership-labelled container while preserving this bind-mounted tree
and stable hostname. This normally keeps the official client's login state,
but WeCom may require QR, phone confirmation, or SMS verification again.

Each initialized profile has a mode `0600` `wecom-profile.json` in its account
state directory and a matching mode `0600` `.wechatcopilot-profile.json`
sentinel inside `wecom/android-data`. The versioned records bind the opaque
account ID, canonical data path, persistent inode, random stable profile
identity, and creation identity. The on-disk identity deliberately excludes
`st_dev`, which can change when the verified dm-crypt state filesystem is
remounted; each sensitive operation still pins and compares the live
device/inode pair. A missing, exchanged, symlinked, or in-place-cleared `/data`
directory, or a missing or mismatched internal sentinel, is a hard startup
failure. The daemon never
creates an empty replacement for a marked profile.

The first activation after upgrading from a markerless release writes both
records only when the existing data directory is real and the existing
container name, account labels, pinned image, hostname, isolated network, and
exact `/data` bind source all verify. A running legacy container must also prove
the live mounted inode. A stopped markerless container is never trusted on
first use: while the daemon is offline, the operator must explicitly run
`wechatcopilot accounts approve-legacy-wecom-profile --account ACCOUNT_ID
--confirm`. That command publishes only a mode-`0600`, one-use approval bound to
the resolved WeCom account, canonical data path and inode, immutable
container ID, and complete stopped execution epoch; activation consumes it
before writing either marker. Running or otherwise changing the container
invalidates and revokes the stale approval. Neither path modifies existing
Android data. Do not remove the legacy container before this first upgraded
activation. Markerless data without the applicable proof is left untouched.

The daemon obtains login screenshots with a bounded, exact-container
`docker exec /system/bin/screencap -p` and submits numeric verification codes
only to the single semantic editable field advertised by the accessibility
tree. It never attempts to bypass a risk or device-verification screen.

## Companion protocol

The companion listens only on Android loopback port `18765`. It generates and
persists its own app-private Bearer token. The daemon reads that fixed private
file through the exact, ownership-verified container and sends bounded raw
HTTP over `docker exec -i /system/bin/toybox nc 127.0.0.1:18765`; the token is
stdin data and never appears in a process argument, environment variable,
Docker inspection record, or host listening socket.

Available endpoints are `GET /v1/health`, `GET /v1/snapshot`,
`GET /v1/events`, and `POST /v1/actions`. Actions are restricted to semantic
click, check-if-unchecked, text entry, forward/backward scroll, Android Back,
and opening a stored notification PendingIntent. The check-only action requires
a fresh visible, enabled, clickable control and rejects an already accepted
control. The pinned WeCom 5.0.9 login screen permits only its exact
`android.widget.ImageView` agreement control
(`com.tencent.wework:id/ow`) while an explicit `selected=false` is present in
the companion snapshot; every other image and a missing state field are
rejected, and that control is always rejected by the generic click action.
Another client layout requires a separately reviewed, version-specific profile;
there is no generic checkbox fallback.
There is no arbitrary ADB, shell, coordinate,
filesystem, WebView debugging, or network proxy method.

Before every exec or copy, the daemon rechecks the exact container name,
account labels, digest-pinned image reference, stable hostname, `/data` mount,
isolated network, and absence of all host port bindings. An older runtime with
published ports is rejected and must be removed and recreated; it is never
silently reused.

## Build

Use JDK 17 and Android SDK 35:

```sh
cd android
./gradlew :companion:testDebugUnitTest :companion:licensee :companion:assembleRelease
```

The unsigned release APK is written to
`companion/build/outputs/apk/release/companion-release-unsigned.apk`. Signing
is a release-pipeline responsibility; signing keys must not enter the source
tree.
