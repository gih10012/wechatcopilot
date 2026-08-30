# CLI reference

The CLI writes one JSON envelope to stdout for ordinary commands and logs to stderr:

```json
{"schema_version":"1","ok":true,"data":{}}
```

Failures use `ok:false` with a stable error code. Streams use one JSON object per line.

## Runtime and accounts

```bash
wechatcopilot doctor --json
wechatcopilot daemon serve
wechatcopilot accounts add --platform wechat --alias personal --json
wechatcopilot accounts list --json
wechatcopilot accounts status --account ACCOUNT_ID --json
wechatcopilot accounts activate --account ACCOUNT_ID --json
wechatcopilot accounts deactivate --account ACCOUNT_ID --json
wechatcopilot accounts adopt-legacy-index --account ACCOUNT_ID --confirm --json
wechatcopilot accounts approve-legacy-wecom-profile --account ACCOUNT_ID --confirm --json
wechatcopilot accounts remove --account ACCOUNT_ID --confirm --purge --json
wechatcopilot capabilities --account ACCOUNT_ID --json
```

All login commands are user-only. Never execute `accounts login` or any `auth` subcommand through an agent shell or tool because their inputs or outputs can contain a bearer login URL or verification secret that would enter model or tool history. Tell the user to type the appropriate `accounts login` command in a separate trusted terminal and never ask them to paste its output:

```bash
wechatcopilot accounts login --account ACCOUNT_ID --wait
# Private LAN only when needed:
wechatcopilot accounts login --account ACCOUNT_ID --lan --lan-address RFC1918_IP --wait
```

The user must open the one-time URL and complete the challenge directly. The page may require the user to explicitly confirm a narrowly allowlisted official-client onboarding consent; an agent must never click it or call its private page endpoint. Do not pass a verification code in an argument or environment variable.

Omit `--lan` for loopback-only login. With `--lan`, omit `--lan-address` to prefer the default-route interface or provide an exact RFC1918 IPv4 assigned to an eligible local interface. Never use a public, wildcard, loopback, container-bridge, or unassigned address.

`accounts remove` always and permanently deletes the deactivated account's profile, runtime state, and message index. It requires both `--confirm` and `--purge`; there is no unregister-without-purge mode in v0.1. If the command returns a retryable `CONFLICT` with `deleting:true`, repeat that exact removal after checking the account ID. Do not activate or use the account while deletion is pending.

`accounts adopt-legacy-index` is an operator-authorized, offline upgrade for an existing non-empty index created before account ownership metadata existed. Stop the daemon first and pass the exact saved account ID or alias plus `--confirm`. The command validates the configured state-mount gate, acquires the daemon's state lock, resolves the account through the existing registry, and validates the complete legacy schema and database integrity at that account's exact index path. It never creates a missing registry, account directory, or legacy index. A running daemon, unknown account, empty/already-owned database, unrecognized schema, unsafe path, or missing state returns a stable error without adoption. Successful output contains only the resolved account ID, alias, platform, and `adopted:true`; indexed content is preserved and not printed. This maintenance command is not exposed through MCP and should run only after explicit operator authorization.

`accounts approve-legacy-wecom-profile` is the corresponding offline, operator-only escape hatch for an existing stopped WeCom container whose non-empty Android data lacks external profile metadata. Stop the daemon, verify the exact saved WeCom account ID or alias, and pass `--confirm`. The command proves the digest-pinned container's exact account labels, hostname, isolated network, `/data` bind, stopped state, and same immutable container ID and execution epoch before and after publication. It creates only a mode-`0600`, one-use approval bound to that account, the canonical data path and inode, and the stopped container's ID, state, start/finish timestamps, restart count, and exit code; it never starts or stops a container or creates/changes `/data`, metadata, or its internal sentinel. The durable approval does not bind `st_dev`, which can change across a verified encrypted-volume remount, but each creation and consumption frame still pins and compares the live device/inode. A valid internal sentinel from an interrupted publication is accepted as recoverable state but never replaces approval. The next exact stopped-container migration consumes the approval before writing either marker; running or changing the container first rejects and revokes it. Running, missing, empty, symlinked, exchanged, already externally marked, wrong-platform, or unregistered state fails closed, and a failed post-publication frame removes only the exact approval it just created. Successful output contains only the resolved account ID, alias, platform, and `approved:true`. This maintenance command is not exposed through MCP and agents must not infer approval from ordinary activation requests.

For an active runtime, `capabilities` returns every stable capability key with one of `stable`, `beta`, `experimental`, or `unsupported`: `auth.qr`, `auth.sms`, `messages.visible`, `messages.history`, `messages.watch`, `messages.send`, `attachments.send`, `official_accounts.read`, `web.open`, `miniprogram.open`, `miniprogram.open_by_name`, `surface.act`, and `surface.assets.export`. Treat a missing or unknown key from an active runtime as a client/daemon contract mismatch. `miniprogram.open` covers only a message-provided surface reference; exact-name launch requires `miniprogram.open_by_name`. An inactive account can return an empty map because it has no current driver; a stale-index read does not depend on or claim live capabilities.

## Conversations and messages

```bash
wechatcopilot conversations list --account ACCOUNT_ID --unread --json
wechatcopilot conversations search --account ACCOUNT_ID --query QUERY --json
wechatcopilot messages history --account ACCOUNT_ID --conversation CONVERSATION_ID --limit 50 --json
wechatcopilot messages history --account ACCOUNT_ID --conversation CONVERSATION_ID --latest --limit 50 --json
wechatcopilot messages watch --account ACCOUNT_ID --cursor CURSOR --follow --jsonl
```

Use opaque IDs returned by the service. A title is display data, not an addressing key.

By default, `messages history` returns the first matching messages after `--cursor` in ascending local sequence order. With `--latest`, it instead selects the newest `--limit` matching messages and then returns that window in ascending sequence order. The CLI rejects `--latest` together with a nonzero `--cursor`; drop `--latest` when paging forward from a saved cursor.

Capability keys describe the available data guarantee, not one subcommand per key. `messages.visible` has no separate CLI command: when `messages.history` is `unsupported` but `messages.visible` is available, use `messages history` to refresh and return the bounded current UI view. Newly observed personal WeChat UI messages report `source:"ui"` and `complete:false`; do not present them as historical completeness. Refuse the read only when both capabilities are `unsupported`.

## Transactional send

```bash
wechatcopilot messages prepare-send \
  --account ACCOUNT_ID \
  --conversation CONVERSATION_ID \
  --text TEXT \
  --json

wechatcopilot messages commit-send \
  --transaction TRANSACTION_ID \
  --idempotency-key UNIQUE_KEY \
  --confirm \
  --json
```

Use a freshly generated idempotency key for each logical send. If commit returns `SEND_UNCERTAIN`, inspect the conversation before taking another action and never blind-retry.

## Surfaces

```bash
wechatcopilot surfaces open --account ACCOUNT_ID --ref SURFACE_REF --json
wechatcopilot surfaces open --account ACCOUNT_ID --mini-program CAMPUS_APP --json
wechatcopilot surfaces snapshot --account ACCOUNT_ID --surface SURFACE_ID --without-image-data --json
wechatcopilot surfaces snapshot --account ACCOUNT_ID --surface SURFACE_ID --screenshot-out SNAPSHOT.png --json
wechatcopilot surfaces actions --account ACCOUNT_ID --surface SURFACE_ID --json
wechatcopilot surfaces act --account ACCOUNT_ID --surface SURFACE_ID --action ACTION_ID --text TEXT --json
wechatcopilot surfaces act --account ACCOUNT_ID --surface SURFACE_ID --action ACTION_ID --confirm --json
wechatcopilot surfaces back --account ACCOUNT_ID --surface SURFACE_ID --json
wechatcopilot surfaces export --account ACCOUNT_ID --surface SURFACE_ID --asset-token ASSET_TOKEN --output CROP.png --json
wechatcopilot surfaces close --account ACCOUNT_ID --surface SURFACE_ID --json
wechatcopilot surfaces share --account ACCOUNT_ID --surface SURFACE_ID --conversation CONVERSATION_ID --json
```

`--ref` and `--mini-program` are mutually exclusive. Use `--ref` only with the opaque `surface_ref` returned on a message; a `message_id` is not a surface reference. The reference remains bound to the exact indexed card locator, so a same-label replacement makes an old reference stale without clicking. Use `--mini-program` with the exact display name. No exact candidate returns `NOT_FOUND`, multiple exact candidates return `TARGET_AMBIGUOUS`, and an exact label outside a provable mini-program section returns `CLIENT_INCOMPATIBLE`; none of those cases clicks a result. Success requires a newly observed verified WMPF window and a returned surface whose kind is actually `miniprogram`; page text alone cannot prove the kind. Do not claim an AppID unless `surface.app_id` is non-empty.

If a daemon restart leaves a verified WMPF or XWeb window focused, named launch may focus the unique official main WeChat window without closing the old surface or changing its contents; multiple main windows fail closed. `surfaces close` sends a window-manager close request only to the twice-verified X11 instance bound to that surface and waits for that exact instance to disappear. It never substitutes a page-level Back or arbitrary Close control.

`open`, `snapshot`, `act`, and `back` return the semantic surface plus its matching PNG screenshot by default. Add `--without-image-data` to omit screenshot base64, or `--screenshot-out NEW_FILE` to verify the digest and securely create a regular, single-link, exact mode-`0600` PNG without overwriting an existing file. The existing parent directory must be owned by the caller and must not be group- or other-writable; its ancestry must likewise prevent an untrusted UID from replacing the path (a root-owned sticky temporary directory containing the caller's private directory is accepted). Use a private runtime or project directory rather than a shared directory. The CLI reserves that exact private output before contacting the daemon, so an invalid path cannot hide a completed action; a later write or validation failure removes only that reserved inode. Surface fields are capability-dependent: current personal WeChat includes generation, screenshot digest, viewport, OCR text, elements, rendered assets, and actions, while current WeCom may include only the applicable screenshot, generation, title, and actions.

The dynamic fields are the agent contract. When present, `elements[]` contains `id`, `target_id`, label/description/role, observational `bounds`, `source`, `confidence`, and its current `action_ids` (`action_id` only when exactly one remains). `actions[]` contains `id`, `target_id`, `label`, `kind`, `risk`, `effect`, and `disabled`. On drivers with `surface.assets.export`, `assets[]` contains `id`, short-lived `token`, `kind`, label, observational `bounds`, `source`, `confidence`, and `expires_at`. Current personal-WeChat snapshots include one `kind:"rendered_viewport"` asset for the complete bound window, so a Canvas-only page still has a controlled rendered export; additional semantic image assets may provide tighter regions. Its `viewport` contains `x`, `y`, `width`, and `height`. Current WeCom does not advertise asset export and may omit `elements`, `assets`, and `viewport`; use its screenshot and advertised actions without inventing those fields. Optional title, URL, AppID, or OCR text may be absent; never invent them. `surfaces actions` is only a compact fresh-action listing and omits the screenshot and other target context needed to interpret an unfamiliar page, so use `snapshot` for the general loop.

Follow `open -> snapshot -> inspect -> act -> snapshot` for every mini program. The action list is observational, not an app-specific script. Use only an action ID re-advertised by the latest snapshot. When elements are present, match `elements[].target_id` to `actions[].target_id`; each element's `action_ids` lists its active actions, and `action_id` is present only as a single-action convenience. Repeated labels require available target IDs and bounds for disambiguation. Never invent missing elements, turn bounds into click coordinates, or construct an action ID or locator.

Low-risk reads, scrolling, proven navigation, and proven search input may run without `--confirm`. Medium-risk, unknown-risk, and `external_write` actions return `CONFIRMATION_REQUIRED` unless `--confirm` reflects the user's explicit current-turn authorization for that exact action. The agent must not add it merely to make a command succeed. Likes, comments, favorites, follows, publishes, submissions, and shares are external writes. High, sensitive, and destructive actions return `USER_ACTION_REQUIRED` regardless of confirmation.

For an advertised `kind:"input"` action, pass replacement text with `--text`; omitting the flag is rejected, while an explicit `--text ""` clears the verified editor. Supplying `--text`, including an explicit empty value, to a non-input action is also rejected. A Canvas/OCR-derived input normally has `risk:"medium"` and `effect:"unknown"`, so its one `surfaces act` call must carry both `--text` and user-authorized `--confirm`. The driver first revalidates the OCR region, then requires exactly one focused editable accessibility target and verifies the replacement value by readback. Confirmation never turns an arbitrary visual region into an input and never bypasses those checks.

`surfaces export` requires `surface.assets.export` and a short-lived token from the latest snapshot. It creates only a normalized `image/png` crop with `fidelity:"rendered"` from that exact screenshot. The `rendered_viewport` token exports the full bound window rather than an arbitrary coordinate range. It cannot return an original mini-program image, media URL, attachment, or official download. A new snapshot replaces the token set, and an expired or mismatched token fails.

## Stable error codes

| Code | Required response |
| --- | --- |
| `AUTH_REQUIRED` | Stop and ask the user to run the login flow manually outside agent context. |
| `AUTH_EXPIRED` | Ask the user to create a new challenge manually; do not reuse the old page. |
| `CONFLICT` with `deleting:true` | Retry only the exact `accounts remove --confirm --purge` operation. |
| `ACCOUNT_INACTIVE` | Activate the exact account or use its last indexed data only. |
| `DRIVER_UNAVAILABLE` | Run `doctor`; report the failed driver dependency. |
| `CLIENT_INCOMPATIBLE` | Stop automation and report the detected client version. |
| `TARGET_AMBIGUOUS` | Stop without clicking and ask the user to refine the exact target or name. The current API does not return a candidate list. |
| `UNSUPPORTED_CAPABILITY` | Do not emulate the action through raw input. |
| `CONFIRMATION_REQUIRED` | Obtain current-turn authorization for the exact prepared send or exact surface action. |
| `SEND_UNCERTAIN` | Do not retry automatically. Inspect the conversation. |
| `PARTIAL_FAILURE` | Report successful and failed parts separately. |
| `USER_ACTION_REQUIRED` | Stop and hand the visible step to the user. |

Run `wechatcopilot <group> <command> --help` for the installed version's authoritative flags. If help output differs from this reference, follow the installed CLI and report the version mismatch.
