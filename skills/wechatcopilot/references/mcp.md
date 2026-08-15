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
- Surface tools open a message's opaque `surface_ref`, snapshot, act on current semantic actions (including an offered back action), and close webpage or mini-program surfaces. Never pass `message_id` as the reference; prepare sharing through the send transaction tools.

Tool names and schemas are discoverable from the running server. Prefer discovery over hard-coding a tool revision. Every account-scoped tool requires `account_id` when multiple accounts exist.

For a bounded initial history read, call `messages_list` with `latest:true` and a limit. The daemon applies the message filters, selects the newest matching window, and returns that window in ascending local sequence order. Do not combine latest mode with a nonzero `after_sequence`; the daemon rejects that ambiguous request. Without latest mode, `after_sequence` reads forward from the cursor.

Capability keys describe data guarantees rather than separate tools. If `messages.history` is `unsupported` but `messages.visible` is available, `messages_list` is still the documented transport for the bounded current UI view. Preserve its `source:"ui"` and `complete:false`; stop only when both read capabilities are `unsupported`.

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

MCP does not accept QR images, phone confirmations, verification codes, account passwords, or database keys. If an account reports `AUTH_REQUIRED`, direct the user to `wechatcopilot accounts login` in a trusted terminal.
