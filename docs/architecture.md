# Architecture

`wechatcopilot` is a local control plane around official WeChat and WeCom clients. It does not implement either private network protocol. Agents see stable account, conversation, message, send-transaction, and surface abstractions; fragile client automation stays behind driver boundaries.

## Components

```text
Agent / operator
  |  JSON CLI or stdio MCP
  v
wechatcopilot CLI
  |  Unix socket (same UID only)
  v
wechatcopilotd
  |- account manager and platform activation locks
  |- capability registry and policy checks
  |- send transaction manager and idempotency journal
  |- SQLite message index
  `- driver boundary
       |- wechat-linux: official client + Xvfb + AT-SPI + XTest
       |                (versioned WCDB reader is a future optional adapter)
       `- wecom-android: Redroid + official APK + local companion
```

The CLI and MCP server share the daemon's business layer. Neither interface receives an arbitrary click, shell, ADB, SQL, or JavaScript primitive.

## Account isolation

Every account has an opaque local UUID and a separate state directory containing its official-client profile, stable runtime identity, index, and compatibility record. One account per platform can be active at a time; WeChat and WeCom occupy independent slots and can run concurrently.

Activation is serialized per platform:

1. Reject writes to the account being stopped.
2. Ask its driver to stop and flush local state.
3. Release its profile lock and runtime mounts.
4. Lock and mount the requested profile.
5. Start the new driver and report its observed authentication state.

Inactive profiles remain queryable at their last indexed point but do not receive live messages. A daemon restart restores the last requested platform slots. If a transient dependency failure prevents startup, the requested slot remains durably active with a degraded status so an explicit activation or later daemon restart can retry it; restore never silently turns it into an inactive profile. The manager never starts two clients against the same profile.

Account removal is a durable two-phase transition. The registry first records `deleting:true` and makes the account unusable, then the driver purges its stopped runtime and the account store removes its state. A failed cleanup leaves the marker in place across restarts; repeating the same confirmed purge resumes cleanup. Only the account list exposes an account in this state.

## State layout

The default root is `${XDG_STATE_HOME:-$HOME/.local/state}/wechatcopilot`; `WECHATCOPILOT_HOME` overrides it. The runtime root is under `${XDG_RUNTIME_DIR}/wechatcopilot`.

```text
state root/
  accounts.json
  downloads/
  accounts/
    ACCOUNT_UUID/
      index.sqlite3
      client-home/          # WeChat profile, when this is a WeChat account
      client-files/         # WeChat file storage
      wecom/.../android-data/ # WeCom /data volume, when this is a WeCom account

runtime root/
  wechatcopilot.sock
  accounts/ACCOUNT_UUID/
  auth/CHALLENGE_UUID/
```

Account state directories and databases are mode `0700`/`0600`. The generated login-link QR file is runtime-only and removed on completion or expiry; official-client login screenshots remain in memory while a challenge is active. See [Security](security.md) for the data and trust model.

An operator may pin the state root to an exact mounted device, filesystem type, and filesystem UUID. When configured, the daemon validates that mount and holds a guard on it before `Paths.Ensure` can create any directory; `doctor` holds the same guard through all state checks. `daemon install` copies the three exported constraints into a required `state-mount.environment` loaded after the optional general environment file. The persisted environment file or its required unit reference also acts as a downgrade marker, so `doctor`, foreground `daemon serve`, and later installs fail when a new shell omits the constraints. This fail-closed gate prevents a missing encrypted mount, forgotten exports, or a general-setting override from producing an unencrypted fallback registry or profile tree.

The supported file-backed layout keeps NTFS3 outside the live-state boundary:

```text
outer NTFS3 filesystem (pinned outer UUID)
  `- fully allocated 64 GiB state.luks (LUKS2)
       `- /dev/mapper/wechatcopilot-state
            `- ext4 mount (pinned inner UUID) -> state root
```

The outer UUID is required by every mutating provisioning operation; the daemon gate uses the distinct inner ext4 UUID. Generated systemd units are `noauto` and use a manually entered passphrase. The workflow does not configure a key file or TPM automatic unlock. Locking requires the daemon and all project containers to be stopped first. Raw or unencrypted disk-backed swap produces a confidentiality warning; operators may opt into a startup-blocking strict policy.

## Driver contract

A driver owns exactly one active official-client runtime. It reports lifecycle state and a complete capability map where each stable key is `stable`, `beta`, `experimental`, or `unsupported`. The shared keys cover QR/SMS authentication, visible/history/watch message reads, text and attachment sends, official-account reads, web and mini-program opens, and surface actions. Identity and exact client version are capability-dependent observations and may be absent when the official UI does not expose them reliably.

The shared contract covers:

- `Start`, `Stop`, `Status`, authentication snapshot, and user-provided auth code submission.
- Conversation listing, bounded message reads, and provenance-bearing message records.
- Sending a resolved semantic request and returning verified or uncertain delivery.
- Opening, snapshotting, acting on, and closing a webpage or mini-program surface.

Driver results use local opaque IDs at the API boundary. Platform IDs and raw payloads remain internal. `complete` describes whether the driver can prove completeness for that result; `confidence` and `source` preserve whether content came from a database, accessibility tree, notification, or visual observation.

## WeChat Linux driver

The driver runs the operator-supplied official Linux AppImage inside an isolated virtual HOME and persistent client profile. `doctor` and driver startup both verify its independently configured SHA-256 before use. Xvfb, a lightweight window manager, a session D-Bus, and a dedicated AT-SPI bus make the official Qt UI available without a physical display.

The current baseline observes visible content through AT-SPI and uses bounded XTest, clipboard, or XDND input only behind semantic driver operations. Screenshots and OCR are fallbacks for surfaces that accessibility cannot describe. A future, explicitly versioned WCDB adapter may add read-only structured history, but no such adapter is implemented or enabled in the current source tree.

Sending validates the account and conversation before input, requires a unique active window, header, editor, and send control, and then looks for a visible outgoing bubble in the official UI. This viewport observation is not a server acknowledgement or local-message-store proof. Lack of visible confirmation becomes `SEND_UNCERTAIN`; it is not retried. Webpages and mini programs stay inside the official client/WMPF runtime and expose only snapshot-derived semantic actions. Each action locator binds the active surface generation to a semantic node signature; any page, path, role, label, geometry, action, or editable-state change makes it stale before input occurs.

## WeCom Android driver

The driver runs the operator-supplied official WeCom APK in a per-account Redroid `/data` volume. A project-owned Android companion uses `AccessibilityService`, `NotificationListenerService`, and screenshot APIs to expose a narrow in-container RPC surface.

The daemon reaches the companion through bounded Docker exec calls, with no published control ports. The driver never uses TLS interception, Frida, Root hiding, or anti-emulator measures. If the official client rejects the environment or displays an account-risk warning, the driver stops and reports the condition.

Android UI history cannot generally prove complete server history, so partial results explicitly return `complete:false`. This driver remains experimental until its compatibility gate passes on an unmodified official client.

## Message and send data flow

Observed driver messages are normalized, assigned local IDs and monotonic sequences, deduplicated, and committed to SQLite/FTS5. Streaming APIs read this journal by cursor rather than holding an infinite request open.

Outbound messages use a two-phase transaction:

1. `prepare-send` resolves the account and conversation, preserves the exact content, records warnings, and creates a short-lived transaction.
2. `commit-send` verifies the transaction, explicit confirmation, and caller idempotency key before invoking the active driver.
3. The driver verifies the resulting message when possible and the journal records one terminal outcome.

An expired or changed transaction is never committed. A repeated idempotency key returns the original outcome and never creates a second send.

## Authentication flow

Authentication is a user action outside MCP and model context. The daemon can expose a short-lived localhost page, or an explicitly requested private-LAN page, containing only the complete official-client login image and currently applicable controls. Code entry appears only for an observed SMS input. Narrowly allowlisted onboarding actions can appear only on this one-time page; execution revalidates the official package, foreground Activity, visible page semantics, unique accessible targets, explicit user confirmation, and snapshot sequence. WeCom login agreements and login-method selection are separate, sequential confirmations, and an accepted action is never replayed after an uncertain observation. These are not general surface actions and are not exposed through CLI or MCP. LAN auto-selection prefers the active default-route interface; an explicit address must be an RFC1918 IPv4 currently assigned to an eligible non-container interface. Challenges are random, single-use, and time limited. After success the page retains a completion result for about 60 seconds so a waiting user can observe it, then the listener closes.

The profile and runtime identity persist across client, container, daemon, and host restarts. This improves session continuity but cannot prevent Tencent from requiring authentication again.
