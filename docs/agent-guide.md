# Agent guide

Agents should treat `wechatcopilot` as a stateful personal-account workstation, not as a generic messaging API. Use stable IDs, preserve provenance, and separate observation from externally visible actions.

## Initial checks

1. Run `wechatcopilot doctor --json` when deployment health is unknown.
2. List accounts and select the exact opaque account ID.
3. Check account status. Stop on anything other than `ONLINE` for a live operation.
4. Read the capability map. Do not assume parity between WeChat and WeCom or between client versions.

Authentication remains a user-only workflow. Never ask the user to paste an SMS code, QR payload, or one-time login URL into model context.

## Reading

Resolve a conversation before reading its messages. Conversation titles are not unique and are never sufficient for addressing. Preserve `source`, `complete`, and `confidence` in downstream summaries or decisions.

For an initial bounded view, request latest mode with a limit. It selects the newest matching locally indexed messages but returns the selected window in ascending local sequence order, so agents can process it chronologically. CLI latest mode cannot be combined with a nonzero cursor; use cursor mode without latest for forward pagination.

A result from an inactive account is only the last local snapshot. WeCom UI collection commonly reports `complete:false`; explain that limitation instead of calling the result full history.

For ongoing collection, use JSONL watch or bounded MCP polling with its returned cursor. Persist cursors per account. If a cursor expires, resume from the newest safe checkpoint and report the possible gap.

## Sending

Every send is prepared and then committed. Preparation must show the exact account, resolved conversation, exact message, attachments or shared surface, warnings, and expiry.

Commit only when the user authorized that exact action in the current turn. Generate one idempotency key per logical send and retain it across transport retries. A timeout or `SEND_UNCERTAIN` is not permission to call commit again; inspect the conversation first.

Require fresh confirmation for group messages, attachments, surface sharing, or any content that changed after preview. Stop on an ambiguous target even when one candidate looks likely.

## Web and mini-program surfaces

Open the surface by passing the source message's opaque `surface_ref` to `surfaces open --ref`; a `message_id` is not a surface reference. Request a snapshot and choose only from its current semantic action IDs. Snapshot after navigation or data entry because the action set becomes stale.

Do not request arbitrary coordinates, keyboard events, ADB, JavaScript injection, or shell access. Stop before payments, transfers, authorization grants, identity verification, security settings, risk warnings, or an action classified as `USER_ACTION_REQUIRED`.

## Failure behavior

- `AUTH_REQUIRED`: ask the user to start a trusted CLI login challenge.
- `TARGET_AMBIGUOUS`: present the candidates and wait for a choice.
- `CLIENT_INCOMPATIBLE`: stop and report the exact detected client version.
- `UNSUPPORTED_CAPABILITY`: report the boundary; do not find a raw-input workaround.
- `SEND_UNCERTAIN`: report that it may have sent and inspect before any retry.
- `PARTIAL_FAILURE`: describe successes and failures independently.

The bundled [Skill](../skills/wechatcopilot/SKILL.md) contains the compact operational workflow; its references document exact CLI and MCP behavior.
