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
a fresh visible, enabled, clickable, checkable node and rejects an already
checked node. There is no arbitrary ADB, shell, coordinate,
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
