# wechatcopilot

`wechatcopilot` is an unofficial, Linux-only control plane for personal WeChat
and WeCom accounts. It gives local agents a deterministic Go CLI, a Unix-socket
daemon, an MCP server, and a Codex Skill while leaving login and network
communication inside the official clients.

> [!WARNING]
> This repository is an early development baseline, not a stable release. No
> real WeChat or WeCom client version has completed the project's end-to-end
> compatibility and restart-persistence acceptance yet. Start with a disposable
> profile, inspect `capabilities`, and do not use an important account until your
> exact client version passes.

## Current scope

| Capability | WeChat for Linux | WeCom on Redroid |
| --- | --- | --- |
| Isolated saved profiles and multi-account switching | Implemented; one active WeChat account | Implemented; one active WeCom account |
| QR / phone / SMS login | UI-derived, beta | Accessibility + screenshot, experimental |
| Conversation and message reads | Visible viewport only; partial | Notification-derived only; partial |
| Historical message adapter | Not implemented | Not implemented |
| Official-account history | Unsupported without a versioned WCDB adapter | Not applicable |
| Text send | Beta UI path | Experimental UI path |
| Attachment send | Experimental | Unsupported |
| Message-backed webpages / mini programs | Experimental semantic actions | Experimental semantic actions |
| Session persistence | Profile is persisted; real restart acceptance pending | Android `/data` is persisted; real restart acceptance pending |

The two platforms may run concurrently. Activating a second account on the same
platform stops the first while retaining its profile and local index. Tencent
may invalidate any saved session and require the user to authenticate again.

The project deliberately does **not** include private-protocol emulation,
traffic interception, anti-detection bypasses, cloud phones, arbitrary
X11/ADB/shell/click APIs, mass messaging, payments, or official-account and
mini-program publishing administration.

## Architecture

```text
Agent -> Skill / MCP / CLI -> local Unix socket -> policy service
                                                   |-- WeChat Linux container
                                                   `-- WeCom Redroid container
```

- Official clients own credentials, login state, and Tencent network traffic.
- Each account receives separate persistent and runtime directories.
- WeChat runs in a headless X11 container. WeCom runs in a persistent Redroid
  container with a small project-owned accessibility companion.
- WeCom exposes no host ADB or companion port. The daemon uses bounded
  `docker exec` requests to Android loopback.
- Sends use `prepare -> inspect -> confirmed commit`, a persistent idempotency
  journal, and no automatic retry after an uncertain UI result.
- Surface actions use opaque semantic IDs. Payment, transfer, authorization,
  identity, account-security, and other high-risk screens stop for the user.

See [architecture](docs/architecture.md), [security model](docs/security.md),
and [agent guide](docs/agent-guide.md) for the detailed boundaries.

## Requirements

- A headless Linux host with a Linux filesystem supporting Unix permissions,
  advisory locks, and SQLite WAL, with at least 30 GiB free for real profiles.
  When capacity exists only on local NTFS3, use NTFS3 only as the outer storage
  for the provisioning workflow's fully allocated 64 GiB LUKS2 image; the live
  state root is the inner ext4 mount, never NTFS3 itself.
- Raw or unencrypted disk-backed swap is supported but can retain decrypted
  account data. `doctor` reports it as a warning by default; deployments that
  prefer blocking over normal swap behavior can set
  `WECHATCOPILOT_STRICT_SWAP=true`. The encrypted state workflow remains
  manual-passphrase only and does not configure TPM automatic unlock.
- Docker Engine accessible by the daemon user. Docker access is effectively
  root-equivalent and belongs in the trusted computing base.
- An official WeChat AppImage and independently verified SHA-256.
- For WeCom, Binder/BinderFS, a locally available Redroid image pinned by
  digest, an official WeCom APK with an independently verified SHA-256, and the
  project-built companion APK.
- A phone only for Tencent's QR scan, phone confirmation, or SMS verification.
  No graphical Linux session or continuously connected external device is
  required.

Official Tencent packages, account profiles, QR codes, screenshots, messages,
database material, and signing keys must never be committed to this repository
or attached to a release.

## Quick start

Build the Go control plane and run its preflight:

```bash
git clone https://github.com/gih10012/wechatcopilot.git
cd wechatcopilot
go build -trimpath -o bin/wechatcopilot ./cmd/wechatcopilot
./bin/wechatcopilot doctor --json
```

Configure the verified official packages and local runtime images as described
in the [installation guide](docs/install.md), then start the daemon:

```bash
./bin/wechatcopilot daemon serve
```

In another terminal, add and activate a profile using the returned opaque ID:

```bash
./bin/wechatcopilot accounts add --platform wechat --alias personal --json
./bin/wechatcopilot accounts activate --account ACCOUNT_ID --json
```

Run the login command yourself in a separate trusted terminal. Never ask an
agent to execute it or paste its output into an agent conversation because the
output contains a one-time bearer URL:

```bash
./bin/wechatcopilot accounts login --account ACCOUNT_ID --wait --json
```

The login command prints a one-time loopback URL. Use `--lan` only on a trusted
private network when the page must be opened from another device; an optional
`--lan-address` must be an RFC1918 address actually assigned to an eligible
local interface. The page shows the complete official-client image and
conditionally offers only the current SMS input or a narrowly allowlisted,
user-confirmed onboarding consent. QR images, verification codes, and those
consent actions stay outside MCP and model context.

After login, inspect real support before an operation:

```bash
./bin/wechatcopilot accounts status --account ACCOUNT_ID --json
./bin/wechatcopilot capabilities --account ACCOUNT_ID --json
./bin/wechatcopilot conversations list --account ACCOUNT_ID --json
./bin/wechatcopilot messages history --account ACCOUNT_ID --conversation CONVERSATION_ID --latest --limit 50 --json
```

`messages history --latest --limit N` selects the newest `N` matching locally
indexed messages, then returns that bounded window in ascending local sequence
order. Omit `--latest` when paging forward with `--cursor`; a nonzero cursor and
`--latest` are intentionally rejected by the CLI.

Send only through a confirmed transaction:

```bash
./bin/wechatcopilot messages prepare-send \
  --account ACCOUNT_ID \
  --conversation CONVERSATION_ID \
  --text 'hello' \
  --json

./bin/wechatcopilot messages commit-send \
  --transaction TRANSACTION_ID \
  --idempotency-key UNIQUE_KEY \
  --confirm \
  --json
```

If commit returns `SEND_UNCERTAIN`, inspect the conversation and do not retry
blindly: the official client may already have sent the message.

## Agent integration

Start the stdio MCP server with:

```bash
./bin/wechatcopilot mcp serve
```

The MCP process delegates to the same daemon and intentionally exposes no login
code, raw X11, ADB, database, container, or shell tool. The repository's
[`wechatcopilot` Skill](skills/wechatcopilot/SKILL.md), also included in Linux
release archives, adds account-selection, capability, confirmation,
partial-read, and uncertain-send policy for cooperative agents.

MCP and the Skill are behavioral guardrails, not a same-UID security sandbox.
Every process running as the daemon's Unix user can access that user's files
and socket. Run untrusted automation under a separate Unix user; use a VM or a
dedicated host for a stronger boundary.

## Development

```bash
make check
make test
make build
```

The repository includes race-tested Go components, Android unit tests, payload
and secret-release guardrails, dependency-license checks, and release asset
allowlisting. These automated checks do not replace acceptance against real
official-client versions.

Additional documentation:

- [Installation and runtime setup](docs/install.md)
- [Architecture and persistence](docs/architecture.md)
- [Security and threat model](docs/security.md)
- [CLI/MCP operating guide](docs/mcp.md)
- [Contributing](CONTRIBUTING.md)
- [Security reporting](SECURITY.md)

## License and trademark

Project-owned code is licensed under the [MIT License](LICENSE). WeChat, Weixin,
WeCom, and Tencent are trademarks of their respective owners. This project is
not affiliated with, endorsed by, or distributed by Tencent.
