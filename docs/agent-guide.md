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

There are two distinct open paths. A message-backed webpage or mini program requires the source message's opaque `surface_ref` and capability `web.open` or `miniprogram.open`; pass it to `surfaces open --ref` and never substitute `message_id`. The reference binds the exact card observed during indexing, so a stale same-label replacement must fail rather than be rediscovered by label. A named launch requires `miniprogram.open_by_name` and `surfaces open --mini-program` with the exact display name. The two inputs are mutually exclusive. A named launch performs no click when search results are ambiguous, requires a newly observed and verified WMPF window, and must return the actual mini-program context before the agent proceeds. Do not claim an AppID unless the returned `app_id` is populated.

Mini-program screens are not a finite workflow. Repeat this observation loop until the user's read-only goal is satisfied or a policy boundary stops it:

```text
open -> snapshot -> inspect elements/actions -> act(action_id) -> snapshot
```

Snapshot after every action, navigation, input, or scroll. Use only an action ID currently offered for that surface. When elements are available, relate `elements[].target_id` to `actions[].target_id`; `elements[].action_ids` lists the actions for that exact target, and `elements[].action_id` may appear as a convenience only when there is exactly one. Current WeCom snapshots may expose only the matching screenshot and actions, so do not invent a missing element relationship. When labels repeat, distinguish targets with available IDs and observed bounds instead of guessing from label text. Bounds are observational metadata, never input coordinates.

Low-risk observation, scrolling, proven navigation, and proven search input may proceed. A `medium` or `unknown` action, or any action with `effect:"external_write"`, requires the user's explicit current-turn authorization for that exact action. An agent must not set `--confirm` or `confirmed:true` by itself. Likes, comments, favorites, follows, publishes, submissions, and shares are external writes. `high`, `sensitive`, and `destructive` actions are always refused even when the user offers confirmation.

OCR may supplement visible text or advertise an unknown visual action. Use OCR input only when execution can prove one focused editable target and read the value back; otherwise treat it as confirmation-required or unsupported. Never infer success merely because the entered text also appears elsewhere on the page.

When `surface.assets.export` is available, an asset token may export exact rendered pixels from the current snapshot. Current personal-WeChat snapshots include a `rendered_viewport` token for the complete verified window, so a Canvas-only page remains exportable even when no semantic image node exists; semantic image tokens may provide tighter crops. Current WeCom does not advertise this capability and supplies no export token. Rendered assets are not the mini program's original image, URL, attachment, or official download. Snapshotting invalidates older asset tokens. Do not request arbitrary coordinates, raw X11/XTest input, keyboard or mouse events, ADB, JavaScript or shell injection, or a forged locator/action ID, and never work around Tencent verification or risk controls.

## Failure behavior

- `AUTH_REQUIRED`: ask the user to start a CLI login challenge manually in a separate trusted terminal. Never run `accounts login` or an `auth` subcommand through an agent tool, or ask for their bearer-URL/verification-secret input or output.
- `TARGET_AMBIGUOUS`: stop and ask the user to refine the exact target or name; the current API does not return a candidate list.
- `CLIENT_INCOMPATIBLE`: stop and report the exact detected client version.
- `UNSUPPORTED_CAPABILITY`: report the boundary; do not find a raw-input workaround.
- `SEND_UNCERTAIN`: report that it may have sent and inspect before any retry.
- `PARTIAL_FAILURE`: describe successes and failures independently.

The bundled [Skill](../skills/wechatcopilot/SKILL.md) contains the compact operational workflow; its references document exact CLI and MCP behavior.
