# Safety rules

## Read operations

- Use the least invasive source exposed by the active driver.
- Preserve message provenance and completeness in summaries.
- Do not copy message bodies, attachments, screenshots, or account identifiers into general logs.
- Do not expose a local attachment path unless the user explicitly requests the file.

## Send operations

- Address only an opaque conversation ID resolved in the current workflow.
- Show the account, conversation, exact payload, attachments, warnings, and expiry before commit.
- Require current-turn user authorization for a group, attachment, share, or externally visible message.
- Never automatically retry an uncertain send.
- Stop on ambiguity, stale UI state, client incompatibility, or unexpected navigation.

## Surface operations

- Use semantic action IDs from the latest snapshot only.
- Stop before payment, transfer, authorization grant, identity verification, or security settings.
- Never issue arbitrary coordinates, shell commands, JavaScript injection, ADB commands, or raw keyboard events through the agent interface.

## Account risk

- Use only official client packages fetched by the operator.
- Do not use protocol emulation, anti-detection, certificate interception, Frida, Root hiding, or device-identity migration.
- Treat beta and experimental capabilities as explicitly opt-in for important accounts.
- A persisted session is not a guarantee: Tencent may require login again at any time.
