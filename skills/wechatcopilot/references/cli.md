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
wechatcopilot accounts remove --account ACCOUNT_ID --confirm --purge --json
wechatcopilot capabilities --account ACCOUNT_ID --json
```

All login commands are user-only. Never execute `accounts login` or any `auth` subcommand through an agent shell or tool because their inputs or outputs can contain a bearer login URL or verification secret that would enter model or tool history. Tell the user to type the appropriate `accounts login` command in a separate trusted terminal and never ask them to paste its output:

```bash
wechatcopilot accounts login --account ACCOUNT_ID --wait
# Private LAN only when needed:
wechatcopilot accounts login --account ACCOUNT_ID --lan --lan-address RFC1918_IP --wait
```

The user must open the one-time URL and complete the challenge directly. Do not pass a verification code in an argument or environment variable.

Omit `--lan` for loopback-only login. With `--lan`, omit `--lan-address` to prefer the default-route interface or provide an exact RFC1918 IPv4 assigned to an eligible local interface. Never use a public, wildcard, loopback, container-bridge, or unassigned address.

`accounts remove` always and permanently deletes the deactivated account's profile, runtime state, and message index. It requires both `--confirm` and `--purge`; there is no unregister-without-purge mode in v0.1. If the command returns a retryable `CONFLICT` with `deleting:true`, repeat that exact removal after checking the account ID. Do not activate or use the account while deletion is pending.

For an active runtime, `capabilities` returns every stable capability key with one of `stable`, `beta`, `experimental`, or `unsupported`: `auth.qr`, `auth.sms`, `messages.visible`, `messages.history`, `messages.watch`, `messages.send`, `attachments.send`, `official_accounts.read`, `web.open`, `miniprogram.open`, and `surface.act`. Treat a missing or unknown key from an active runtime as a client/daemon contract mismatch. An inactive account can return an empty map because it has no current driver; a stale-index read does not depend on or claim live capabilities.

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
wechatcopilot surfaces snapshot --account ACCOUNT_ID --surface SURFACE_ID --json
wechatcopilot surfaces actions --account ACCOUNT_ID --surface SURFACE_ID --json
wechatcopilot surfaces act --account ACCOUNT_ID --surface SURFACE_ID --action ACTION_ID --json
wechatcopilot surfaces back --account ACCOUNT_ID --surface SURFACE_ID --json
wechatcopilot surfaces close --account ACCOUNT_ID --surface SURFACE_ID --json
wechatcopilot surfaces share --account ACCOUNT_ID --surface SURFACE_ID --conversation CONVERSATION_ID --json
```

Use the opaque `surface_ref` returned on a message; a `message_id` is not a surface reference. The action list is replaced after each state change. Do not reuse an action ID from an earlier snapshot.

## Stable error codes

| Code | Required response |
| --- | --- |
| `AUTH_REQUIRED` | Stop and ask the user to run the login flow manually outside agent context. |
| `AUTH_EXPIRED` | Ask the user to create a new challenge manually; do not reuse the old page. |
| `CONFLICT` with `deleting:true` | Retry only the exact `accounts remove --confirm --purge` operation. |
| `ACCOUNT_INACTIVE` | Activate the exact account or use its last indexed data only. |
| `DRIVER_UNAVAILABLE` | Run `doctor`; report the failed driver dependency. |
| `CLIENT_INCOMPATIBLE` | Stop automation and report the detected client version. |
| `TARGET_AMBIGUOUS` | Ask the user to choose from returned conversation IDs. |
| `UNSUPPORTED_CAPABILITY` | Do not emulate the action through raw input. |
| `CONFIRMATION_REQUIRED` | Obtain user confirmation for the prepared transaction. |
| `SEND_UNCERTAIN` | Do not retry automatically. Inspect the conversation. |
| `PARTIAL_FAILURE` | Report successful and failed parts separately. |
| `USER_ACTION_REQUIRED` | Stop and hand the visible step to the user. |

Run `wechatcopilot <group> <command> --help` for the installed version's authoritative flags. If help output differs from this reference, follow the installed CLI and report the version mismatch.
