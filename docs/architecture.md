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
      wecom-profile.json    # WeCom account/data identity marker, when initialized
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

A driver owns exactly one active official-client runtime. It reports lifecycle state and a complete capability map where each stable key is `stable`, `beta`, `experimental`, or `unsupported`. The shared keys cover QR/SMS authentication, visible/history/watch message reads, text and attachment sends, official-account reads, message-backed web and mini-program opens, named mini-program launch, surface actions, and rendered asset export. `miniprogram.open` accepts only a message-provided surface reference; `miniprogram.open_by_name` is a separate capability for exact-name launch. Identity and exact client version are capability-dependent observations and may be absent when the official UI does not expose them reliably.

The shared contract covers:

- `Start`, `Stop`, `Status`, authentication snapshot, and user-provided auth code submission.
- Conversation listing, bounded message reads, and provenance-bearing message records.
- Sending a resolved semantic request and returning verified or uncertain delivery.
- Opening by message reference or, when supported, exact mini-program name; snapshotting, acting on, exporting a rendered region from, and closing the surface.

Driver results use local opaque IDs at the API boundary. Platform IDs and raw payloads remain internal. `complete` describes whether the driver can prove completeness for that result; `confidence` and `source` preserve whether content came from a database, accessibility tree, notification, or visual observation.

## WeChat Linux driver

The driver runs the operator-supplied official Linux AppImage inside an isolated virtual HOME and persistent client profile. `doctor` and driver startup both verify its independently configured SHA-256 before use. Xvfb, a lightweight window manager, a session D-Bus, and a dedicated AT-SPI bus make the official Qt UI available without a physical display.

The current baseline observes visible content through AT-SPI and uses bounded XTest, clipboard, or XDND input only behind semantic driver operations. Screenshots and OCR are fallbacks for surfaces that accessibility cannot describe. A future, explicitly versioned WCDB adapter may add read-only structured history, but no such adapter is implemented or enabled in the current source tree.

Sending validates the account and conversation before input, requires a unique active window, header, editor, and send control, and then looks for a visible outgoing bubble in the official UI. This viewport observation is not a server acknowledgement or local-message-store proof. Lack of visible confirmation becomes `SEND_UNCERTAIN`; it is not retried.

Webpages and mini programs stay inside the official client/WMPF runtime and expose a general snapshot/action contract rather than per-app procedures. Each snapshot carries a surface generation, screenshot digest, viewport, semantic elements, rendered assets, and allowed actions. Elements and actions share target IDs; element action links are rebuilt from the currently active action set. An action ID and its private locator bind the verified window identity, target-local semantic evidence, and stable surrounding interaction context. Low-risk controls retain target-local freshness so an unrelated timer does not break browsing. Medium, unknown, external-write, and high-risk controls additionally bind the complete semantic generation and rendered-frame digest; any intervening amount, prompt, sibling text, or pixel change invalidates the old confirmation before input. Navigation, verified search input, OCR unknown actions, and scrolling also incorporate the applicable rendered context, so a new page or viewport receives a new action ID and the prior ID stays stale. A changed target path, role, label, geometry, action, editable state, OCR region, or window identity fails closed.

Actions are one-shot capabilities and are consumed before backend dispatch. A private replay identity separately binds the verified window and local target/action without exposing a reusable locator. Successful navigation consumes only its exact contextual ID, allowing a genuinely new page to advertise a new ID for the same local control. Read-only viewport observation and verified replace-style search input also consume only the exact ID and may recover from a fresh snapshot. Every other dispatched mutation permanently tombstones its replay identity even after reported success; navigation, unknown, external-write, and unrecognized actions also receive that tombstone after an error or timeout. Thus a remote pixel change or different contextual ID cannot revive a completed or uncertain write. Medium, unknown, and external-write actions require explicit confirmation; high, sensitive, and destructive actions are never dispatched.

OCR is a fallback for visible text and otherwise unknown visual targets. Visual input proceeds only after the clicked region resolves to one focused editable accessibility target and the inserted value can be read back. Asset export accepts only a short-lived token from the latest exact snapshot and returns normalized rendered pixels from that screenshot. Every snapshot includes a full-window `rendered_viewport` asset, including Canvas-only pages, while semantic image nodes may add tighter assets. Neither recovers an original media URL or source file. Named launch likewise fails closed: the exact search result must be unique, activation must produce a newly observed verified WMPF window, and the resulting surface must verify as a mini program. After a daemon restart, a leftover verified WMPF/XWeb window may be left intact while the driver focuses the unique official main WeChat window; it never guesses between multiple main windows. Closing targets the exact twice-verified bound X11 instance through the window manager and succeeds only after that instance disappears, rather than treating page navigation as closure.

Message-backed opens bind `surface_ref` to a private locator for the exact card node, action, signature, conversation, label, and declared surface kind observed during message indexing. The opaque public reference includes the locator in its digest but never exposes it. Opening re-resolves and revalidates that same locator immediately before activation; if an old node is replaced by another card with the same label, the old reference stays bound to the old signature and produces no click.

## WeCom Android driver

The driver runs the operator-supplied official WeCom APK in a per-account Redroid `/data` volume. A project-owned Android companion uses `AccessibilityService`, `NotificationListenerService`, and screenshot APIs to expose a narrow in-container RPC surface.

The daemon reaches the companion through bounded Docker exec calls, with no published control ports. The driver never uses TLS interception, Frida, Root hiding, or anti-emulator measures. If the official client rejects the environment or displays an account-risk warning, the driver stops and reports the condition.

Android UI history cannot generally prove complete server history, so partial results explicitly return `complete:false`. This driver remains experimental until its compatibility gate passes on an unmodified official client.

Openable WeCom notification references include a digest scoped to the active account and event sequence. The driver validates that scope before contacting the companion, so equal notification sequences in two accounts cannot address each other's surfaces.

## Message and send data flow

Observed driver messages are normalized, assigned local IDs and monotonic sequences, deduplicated, and committed to SQLite/FTS5. Streaming APIs read this journal by cursor rather than holding an infinite request open.

Every index stores a schema version and its owning account UUID inside the
database. Opening it under another account, through a symlink or hard link, or
after its registered account directory disappears fails closed. A recognized
legacy index with no conversation, message, FTS, or send-journal rows can be
bound transactionally on first open. A non-empty pre-metadata index is never
claimed automatically. The operator-only `accounts adopt-legacy-index
--account ACCOUNT_ID --confirm` command must run while holding the daemon's
state lock; it resolves the account through the existing registry, validates
the configured state mount plus the complete legacy schema and integrity at
the exact registered account path, and then writes ownership metadata
transactionally while preserving all rows. It never creates a missing account
directory or legacy index.

Outbound messages use a two-phase transaction:

1. `prepare-send` resolves the account and conversation, preserves the exact content, records warnings, and creates a short-lived transaction.
2. `commit-send` verifies the transaction, explicit confirmation, and caller idempotency key before invoking the active driver.
3. The driver verifies the resulting message when possible and the journal records one terminal outcome.

An expired or changed transaction is never committed. A repeated idempotency key returns the original outcome and never creates a second send.

## Authentication flow

Authentication is a user action outside MCP and model context. The daemon can expose a short-lived localhost page, or an explicitly requested private-LAN page, containing only the complete official-client login image and currently applicable controls. Code entry appears only for an observed SMS input. Narrowly allowlisted onboarding actions can appear only on this one-time page; execution revalidates the official package, foreground Activity, visible page semantics, unique accessible targets, explicit user confirmation, and snapshot sequence. WeCom login agreements and login-method selection are separate, sequential confirmations, and an accepted action is never replayed after an uncertain observation. These are not general surface actions and are not exposed through CLI or MCP. LAN auto-selection prefers the active default-route interface; an explicit address must be an RFC1918 IPv4 currently assigned to an eligible non-container interface. Challenges are random, single-use, and time limited. After success the page retains a completion result for about 60 seconds so a waiting user can observe it, then the listener closes.

The profile and runtime identity persist across client, container, daemon, state-volume remount, and host restarts. WeCom binds the external account marker's registered canonical path and persistent inode to a matching random sentinel inside Android `/data`; a missing directory, exchanged inode, in-place-cleared tree, missing sentinel, or mismatched account/profile identity fails before an empty replacement can start. A durable marker does not treat `st_dev` as stable because reconstructing a verified dm-crypt mapper can legitimately change its device number. Schema-1 markers are upgraded only after the old account, path, inode, and sentinel have been proved through one pinned frame. Initial and legacy publication pins one `/data` inode, performs content classification and sentinel reads through that dirfd, and transactionally rolls back only its own markers if any final canonical check fails. A running legacy container proves one frame as exact-container ID -> pinned canonical host device/inode -> live `/data` device/inode and sentinel -> the same pinned canonical host identity -> the same exact-container ID; subsequent start, stop, cleanup, Android exec, and copy operations target a proved immutable ID rather than its mutable name. A stopped legacy container instead requires a separately created, one-use offline approval bound to the saved account, canonical data path and inode, immutable container ID, and its complete stopped execution epoch (state, start/finish timestamps, restart count, and exit code). Schema-2 approvals retain their old device field only for compatible validation; the current live device is independently pinned when they are consumed. Migration consumes the approval before publishing either profile marker and rejects then revokes it if that container ran or changed after approval. A later independent live proof revokes every structurally valid same-account approval, including one for a previous inode, so it cannot be replayed if old data returns. A valid internal sentinel left by an interrupted publication remains recoverable, but it never substitutes for the running proof or stopped approval. This improves session continuity but cannot prevent Tencent from requiring authentication again.
