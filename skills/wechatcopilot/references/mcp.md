# MCP reference

Start the stdio server with:

```bash
wechatcopilot mcp serve
```

The MCP server delegates to the same local daemon and policy layer as the CLI. It must not have direct X11, ADB, database, or container access.

## Tool groups

- Account tools list accounts, read status, and discover capabilities.
- Conversation tools list and resolve opaque conversation IDs.
- Message tools read history, poll incrementally, prepare a send, and commit an authorized send.
- Surface tools open a message-backed surface or exact-name mini program, snapshot, act on current semantic actions (including an offered back action), export a rendered screenshot region, and close the surface. Never pass `message_id` as a surface reference; prepare sharing through the send transaction tools.

Tool names and schemas are discoverable from the running server. Prefer discovery over hard-coding a tool revision. Every account-scoped tool always requires an exact `account_id`, even when only one account exists.

For a bounded initial history read, call `messages_list` with `latest:true` and a limit. The daemon applies the message filters, selects the newest matching window, and returns that window in ascending local sequence order. Do not combine latest mode with a nonzero `after_sequence`; the daemon rejects that ambiguous request. Without latest mode, `after_sequence` reads forward from the cursor.

Capability keys describe data guarantees rather than separate tools. If `messages.history` is `unsupported` but `messages.visible` is available, `messages_list` is still the documented transport for the bounded current UI view. Preserve its `source:"ui"` and `complete:false`; stop only when both read capabilities are `unsupported`.

## Surfaces

`surfaces_open` accepts exactly one of:

- `reference`: an opaque message `surface_ref`, gated by `web.open` or `miniprogram.open`;
- `mini_program`: an exact display name, gated separately by `miniprogram.open_by_name`.

Never substitute a `message_id` for `reference`. A named launch returns `NOT_FOUND` when no exact candidate exists, `TARGET_AMBIGUOUS` when several exist, and fails without clicking in either case. The current ambiguity error does not include a candidate list, so ask the user to refine the exact name rather than claiming options were returned. Success requires a newly observed verified WMPF window. Verify the returned `kind` and actual mini-program context before proceeding; do not infer a mini program from arbitrary page text or claim an AppID when `app_id` is empty.

Use the open result as the first observation, then repeat `surfaces_snapshot -> inspect -> surfaces_act -> surfaces_snapshot`. There is no fixed mini-program flow. After every input, navigation, or scroll, discard the prior action set and use only an action ID re-advertised by the new snapshot. When elements are present, relate `elements[].target_id` to `actions[].target_id`; `elements[].action_ids` is the complete current relationship, while `elements[].action_id` is only a single-action convenience. Use available bounds to distinguish repeated labels, never as input coordinates, and do not invent elements omitted by a driver.

`surfaces_open`, `surfaces_snapshot`, and successful `surfaces_act` results contain JSON metadata as text/structured content plus the exact matching screenshot as MCP `ImageContent`. Screenshot base64 is not copied into text or structured content. `surfaces_export` similarly returns metadata plus `ImageContent` for one current asset token; it never returns a daemon-local path. The exported bytes are a normalized PNG crop with `fidelity:"rendered"`, not an original image, URL, attachment, or official download. A later snapshot replaces prior asset tokens.

Treat the fields present in one result as one observation; do not infer omitted metadata. Actions expose `id`, `target_id`, `label`, `kind`, `risk`, `effect`, and `disabled`. Current personal-WeChat results also expose `viewport`, `ocr_text`, `elements[]`, and `assets[]`: elements carry their semantic metadata and action links, while assets carry short-lived export tokens. With `surface.assets.export`, each personal-WeChat snapshot includes a generation-bound `kind:"rendered_viewport"` asset for the complete verified window, including Canvas-only pages; semantic image assets may add tighter crops. Current WeCom does not advertise asset export and may omit `elements`, `assets`, and `viewport`; use its matching screenshot and advertised actions. Optional surface identity fields such as title, URL, and AppID may be absent. There is intentionally no separate MCP actions-list tool: use `surfaces_snapshot` so actions remain attached to their screenshot and generation.

Use `surfaces_act.text` only with an advertised input action. The field must be present for input, including as `""` for an intentional clear; omission is rejected rather than interpreted as empty. A non-input action rejects the field even when its value is empty. Set `confirmed:true` only when the user explicitly authorizes that exact action in the current turn. It is required for medium-risk, unknown-risk, and `external_write` actions such as likes, comments, favorites, follows, publishes, submissions, and shares. It never overrides a high, sensitive, or destructive block. Low-risk observation, scrolling, proven navigation, and proven search input need no confirmation. OCR input is usable only when the driver proves one focused editable target and verifies readback.

A Canvas/OCR input advertised as `kind:"input"`, `risk:"medium"`, and `effect:"unknown"` requires one `surfaces_act` call carrying both `text` and user-authorized `confirmed:true`. The Go policy layer consumes confirmation before dispatch; the UI backend receives only the already-authorized semantic locator and replacement text. A successful tool result includes the new snapshot, but the agent must still discard the prior action set and reason only from the returned/new snapshot.

## Polling

Use the bounded message poll tool rather than expecting an unbounded MCP response. Supply its last cursor and persist the returned cursor. A cursor only orders the local account journal; it does not prove complete server history.

## Writes

Send and share are two-phase operations:

1. Prepare with the exact `account_id`, `conversation_id`, and payload.
2. Inspect the returned preview and warnings.
3. Commit the transaction with explicit confirmation and a new idempotency key.

Never call commit after the transaction expires or after its preview no longer matches the request. Do not retry `SEND_UNCERTAIN` automatically.

Account removal is a separate destructive operation. Call `account_remove` only with the exact opaque account ID and only when both `purge:true` and `confirmed:true` are authorized. A failed cleanup leaves `deleting:true`; in that state, retry only the same removal and do not call other account tools.

## Authentication

MCP does not accept QR images, phone confirmations, verification codes, account passwords, or database keys. If an account reports `AUTH_REQUIRED`, direct the user to type `wechatcopilot accounts login` manually in a separate trusted terminal. Never invoke it or any `auth` subcommand through an agent shell or tool, and never ask for their inputs or outputs, because those commands handle a bearer login URL or verification secret.
