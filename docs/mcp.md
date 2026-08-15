# MCP server

`wechatcopilot mcp serve` starts a local stdio MCP server. It is a thin adapter over the daemon and uses the same account locks, policy checks, send transactions, idempotency journal, and error model as the CLI.

## Transport and trust

The supported v0.1 transport is stdio. Start one server per agent process. The server connects to the same-UID daemon Unix socket and does not expose a listening TCP port.

Do not place the server behind a public remote MCP gateway. MCP does not carry verification codes, QR images, phone confirmations, client database keys, raw screenshots from login, or account profile data.

## Tool model

Tools are grouped around accounts, capabilities, conversations, messages, send transactions, and webpage/mini-program surfaces. Inspect the running server's tool schemas rather than assuming a development snapshot.

All account-scoped tools require an opaque `account_id` when more than one account exists. Message tools use opaque `conversation_id` and `message_id` values. Open a message-backed surface with that message's separate opaque `surface_ref`; do not substitute its `message_id`. Surface actions use a short-lived semantic `action_id` from the most recent snapshot.

Read tools return provenance, completeness, and confidence. Consumers must preserve these fields when they affect interpretation.

`messages_list` accepts `latest:true` for a bounded tail read. Filters are applied first, the newest `limit` matching messages are selected, and that window is returned in ascending local sequence order. Do not combine latest mode with a nonzero `after_sequence`; the daemon rejects that ambiguous request. Without latest mode, results begin after `after_sequence` and continue forward.

## Polling

MCP calls are bounded. Use the message poll tool with an account-specific cursor and timeout; persist its returned cursor. Do not use a single never-ending request. Cursors represent ordering in the local journal and do not prove complete remote history.

## Write tools

Sending and surface sharing are two-phase:

1. Prepare the exact action and inspect its preview, warnings, and expiration.
2. Obtain current-turn authorization when needed.
3. Commit with the transaction ID, explicit confirmation, and a caller-generated idempotency key.

Write tools are declared non-read-only. A repeated idempotency key returns its stored terminal result. `SEND_UNCERTAIN` must never trigger an automatic retry.

## Error handling

Errors include a stable code, human-readable message, and optional structured detail. Branch on the code, not the message. Stop and involve the user for authentication, ambiguous targets, client incompatibility, account-risk warnings, high-risk surfaces, or user-action requirements.

See [Agent guide](agent-guide.md) for operational policy and the Skill's [MCP reference](../skills/wechatcopilot/references/mcp.md) for the compact agent workflow.
