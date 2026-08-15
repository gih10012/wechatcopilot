---
name: wechatcopilot
description: Operate personal WeChat and WeCom accounts through the local wechatcopilot CLI or MCP server. Use when an agent needs to inspect account status or capabilities, read or search chats and official-account messages, watch for new messages, prepare and send a message through the user's own account, or open and interact with a web or mini-program surface while preserving account isolation and explicit send confirmation.
---

# WeChat Copilot

Use `wechatcopilot` as the semantic control plane for official WeChat and WeCom clients. Keep authentication and risky confirmation steps with the user; never automate around Tencent verification or account-risk warnings.

## Start every workflow

1. Run `wechatcopilot doctor --json` when runtime health is unknown.
   If `state_mount` or `swap_confidentiality` fails, stop. Never unset mount constraints, substitute an unmounted fallback directory, or bypass the swap gate. Before first real-account authentication, follow the encrypted-volume and swap gates in [account and authentication workflow](references/accounts.md).
2. Run `wechatcopilot accounts list --json` and select the exact `account_id`.
3. Run `wechatcopilot accounts status --account <account_id> --json`.
4. If the account is not `ONLINE`, do not send, watch, refresh visible UI data, or operate surfaces. A bounded read or search may use only its existing local index when the user accepts stale data; label it as an inactive snapshot. For a live operation, activate the exact account and direct the user to the CLI login flow if authentication is required. Never request, receive, or relay an SMS code through the model or MCP.
5. For an `ONLINE` account, run `wechatcopilot capabilities --account <account_id> --json` before relying on a beta, experimental, or platform-specific feature. An inactive stale-index read neither requires nor proves current driver capabilities; use only the stored result provenance and completeness.

When several accounts exist, always pass `--account`; do not infer an account from its alias. Read [CLI reference](references/cli.md) for commands and stable errors. For MCP calls, read [MCP reference](references/mcp.md).

## Read messages

1. Resolve a conversation through `conversations list` or `conversations search`.
2. Select by opaque `conversation_id`, not by title alone.
3. Check both `messages.history` and `messages.visible`. If history is available, read it with `messages history`. If history is `unsupported` but visible messages are available, use the same command to read the bounded current UI view; this is the documented transport for `messages.visible`, not a history-capability bypass. Stop only when both read capabilities are `unsupported`.
4. Use `--latest --limit N` for a bounded initial tail, and preserve `complete`, `source`, and `confidence` in any answer. Latest results are still ordered by ascending local sequence.
5. Treat `complete:false` as a partial client-side view. Personal WeChat UI observations normally report `source:"ui"` and `complete:false`; inactive accounts and WeCom UI collection can also be incomplete.
6. Use bounded `messages watch` or MCP polling for new messages. Persist the returned cursor when operating continuously.

Never claim that locally observed messages represent complete cloud history.

## Send safely

1. Resolve and show the exact account and conversation.
2. Call `messages prepare-send` with the intended content.
3. Present its exact preview, warnings, and expiry to the user unless the user explicitly authorized that exact send in the current turn.
4. Call `messages commit-send` with the transaction ID, a unique idempotency key, and confirmation.
5. Report `SEND_UNCERTAIN` as uncertain. Do not retry automatically because the client may already have sent the message.

Never send using a display name alone. Never reuse an idempotency key for different content. Read [safety rules](references/safety.md) before sending to a group or using an experimental driver.

## Use webpages and mini programs

1. Take `surface_ref` from the source message and pass it to `surfaces open --ref`; never substitute `message_id`.
2. Call `surfaces snapshot` and select one of the returned semantic action IDs.
3. Call `surfaces act`; snapshot again after every navigation or form submission.
4. Use `surfaces share` only through the same prepare/commit send transaction.
5. Close the surface when finished.

Do not invent coordinates or expose X11, ADB, shell, or raw input commands. Stop on `USER_ACTION_REQUIRED`, payment, authorization, account-risk, or identity-verification screens.

## Handle authentication and multiple accounts

Use the CLI login flow outside model context. A user may open its one-time local or LAN page to scan a QR code, confirm on a phone, or enter an SMS code. The code must never appear in command arguments, environment variables, logs, chat, or MCP calls.

Only one account per platform is active at a time. Switching accounts preserves the inactive profile and indexed history but does not collect new messages while that profile is stopped. WeChat and WeCom may each have one active account concurrently. Read [account and authentication workflow](references/accounts.md) before adding, activating, removing, or recovering an account.

## Respect capability and failure boundaries

- Treat `unsupported` as final for that capability and client version. The documented `messages.visible` transport above does not turn visible UI observations into history.
- Describe `experimental` behavior before using it on an important account.
- Stop on `AUTH_REQUIRED`, `CLIENT_INCOMPATIBLE`, `TARGET_AMBIGUOUS`, or any account-risk warning.
- Never bypass Tencent controls, install unofficial protocol gateways, or modify official client traffic.
- Keep message bodies, screenshots, QR codes, auth challenges, local paths, and database material out of logs and final responses unless the user explicitly needs the content.
