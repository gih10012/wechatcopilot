# Security model

`wechatcopilot` controls personal communication accounts and should be treated like a logged-in desktop client. The host, its administrator, the operator Unix UID, and every process running as that UID are part of the trusted computing base. Other Unix UIDs, LAN peers, containers outside this deployment, and all repository or CI infrastructure are untrusted.

## Security boundaries

- The daemon listens on a Unix socket owned by the current UID. There is no general network API.
- Account profiles are isolated by account UUID and cannot be mounted by two runtimes at once.
- Drivers expose semantic operations only. Arbitrary X11 input, ADB, SQL, shell, coordinates, and JavaScript are not public APIs.
- Official WeChat and WeCom clients perform login and network communication. The project does not impersonate their protocols or intercept TLS.
- Authentication challenges, QR codes, screenshots, verification codes, and database keys never pass through MCP.

Root or an administrator of the host can still read process memory, client profiles, screen content, and messages. This project cannot defend against a compromised host.

Unix socket peer credentials and `0600`/`0700` modes isolate different UIDs only. Any process or agent already running as the operator UID can connect to the daemon directly, read permitted state, and call daemon HTTP endpoints that MCP intentionally omits. The MCP schemas and bundled Skill are behavioral guardrails for cooperative agents, not a sandbox against a malicious or compromised same-UID process. Run less-trusted automation under a separate Unix user without access to the socket and state; use a dedicated VM or host when a stronger boundary is required.

## Sensitive data

Long-lived state includes official-client profiles, stable local device identity, normalized message text, conversation metadata, and attachment references. It is stored below `${WECHATCOPILOT_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/wechatcopilot}` with account directories mode `0700` and files mode `0600`.

Use an encrypted filesystem such as LUKS or fscrypt for the state root. Backups contain logged-in client state and message history; encrypt them and restrict recovery access. Restoring a profile to another host may invalidate it or trigger Tencent verification.

The supported NTFS3 fallback uses NTFS3 only outside the encryption boundary: a fully allocated 64 GiB file on NTFS3 contains LUKS2, whose decrypted mapper contains the live ext4 state filesystem. Never point `WECHATCOPILOT_HOME` at NTFS3. The provisioning commands that change system or volume state require the exact outer backing-filesystem UUID and recheck logical versus allocated image size, preventing a similarly named mount or sparse image from controlling the volume. LUKS provides confidentiality here, not protection from deletion, truncation, malicious modification, NTFS damage, or rollback; preserve independent offline backups.

For the inner mounted state root, configure `WECHATCOPILOT_STATE_MOUNT_SOURCE`, `WECHATCOPILOT_STATE_MOUNT_FSTYPE`, and `WECHATCOPILOT_STATE_MOUNT_UUID` together. `doctor`, `daemon serve`, and `daemon install` then fail before creating directories unless the exact filesystem is mounted. `daemon install` persists the three already-exported constraints in a required mode-`0600` `state-mount.environment` loaded after general settings. The persisted file or its unit reference also prevents a later command from silently downgrading the gate when a shell forgets those exports. This prevents a missing encrypted volume or an environment-file override from silently creating a second plaintext profile beneath its mountpoint.

Encryption of the state filesystem does not protect memory pages written to raw or plaintext disk swap. Do not perform a real-account login until the host has no such swap; use no swap, a real zram block device with no disk writeback backing device, or a separately verified dm-crypt swap device. Zram is accepted only when its sysfs `backing_dev` is absent or exactly `none`. The provisioning script warns about non-zram swap, `doctor` exposes unprotected targets as a blocking `swap_confidentiality` check, and the daemon refuses to start or restore saved accounts while the check fails. The volume workflow deliberately configures manual passphrase unlock, rejects conventional mapper key files, and does not enroll or automatically unlock with a TPM.

Before locking the state volume, stop the user daemon and every project-labelled client container. The lock command verifies both conditions before unmounting and closing the mapper; its confirmation flag does not weaken those checks. After changing the outer `/share` automount idle timeout, run `systemctl daemon-reload` before provisioning so a stale finite timeout cannot detach the backing filesystem under the encrypted volume.

Ephemeral challenge material is stored below `${XDG_RUNTIME_DIR}/wechatcopilot`, which should be a per-user tmpfs. Generated login-link QR files are removed after success, expiry, or daemon shutdown. Official-client login screenshots and temporary visual captures are served from memory and are not intentionally persisted.

Verification codes and authentication tokens are secret values:

- If a future versioned message-database adapter needs a database key, keep it in process memory only and never expose it through MCP.
- Accept a verification code only from CLI stdin or the one-time login page, never from arguments or environment variables.
- Redact message bodies, local paths, account identifiers, phone numbers, QR data, and screenshots from ordinary logs.

v0.1 does not implement a durable audit writer. Ordinary logs are redacted operational diagnostics, not an audit trail; do not rely on them for compliance or non-repudiation.

## Login page

Localhost is the default. Private-LAN access requires an explicit `--lan` request. Automatic selection prefers an RFC1918 IPv4 address on the active default-route interface and excludes loopback, down, Docker, veth, and bridge interfaces. `--lan-address` or `WECHATCOPILOT_LAN_ADDRESS` may choose a different address, but the daemon accepts it only when that exact RFC1918 address is currently assigned to an eligible local interface. Public, wildcard, loopback, container-bridge, and unassigned addresses fail before listening.

Each challenge uses at least 96 random bits, expires within ten minutes, and succeeds once. The server limits attempts, uses `Cache-Control: no-store`, and serves no external resources. A completed result remains available for about 60 seconds so the user can observe success; the listener then closes. It is not a VNC console and provides no raw input controls.

The one-time URL is a bearer secret. Run `accounts login` and every low-level `auth` command yourself in a separate trusted terminal; never invoke them through an agent tool or ask an agent to capture their input or output. Do not paste the URL into an agent conversation, public terminal transcript, or third-party URL scanner. Use a trusted private network; the LAN page is not intended to be exposed through port forwarding, a reverse proxy, or the public Internet.

## Agent write policy

Sending and sharing use a short-lived two-phase transaction. Preparation resolves the exact account and opaque conversation ID and returns the exact payload for inspection. Commit requires explicit confirmation and a caller-generated idempotency key.

The daemon rejects nickname-only targets. It does not blind-retry an unverified send. `SEND_UNCERTAIN` means the message may already be visible to recipients and requires manual inspection.

Operations stop on authentication, account-risk, payment, transfer, permission grant, identity verification, ambiguous target, incompatible client, or stale surface state. There is no mass-send, auto-reply, payment, private-protocol, anti-detection, or arbitrary-input feature.

## Container hardening

- Do not publish X11, VNC, companion, or debug ports. WeCom control uses bounded Docker exec calls.
- Pin runtime images by digest and configure independently obtained official-client hashes. WeChat `doctor` and driver startup both fail closed on an AppImage digest mismatch; startup resolves a configured local runtime tag to an immutable image ID before container creation.
- Use separate volumes and container identities per account.
- Refuse existing containers whose actual image, mounts, propagation, environment, resources, ports, networks, capabilities, devices, or namespace settings differ from the account fingerprint.
- Grant Binder and graphics-related privileges only to the Android runtime that requires them.
- Do not mount the Docker socket into either client container.
- Do not mount the repository, SSH keys, cloud credentials, or unrelated home directories into client containers.

Redroid requires unusual kernel/container privileges and therefore has a larger attack surface than the Linux client runtime. Run it on a dedicated trusted host when possible.

## Supply chain and releases

The repository, CI artifacts, images, and GitHub Releases must not contain official WeChat/WeCom binaries, account profiles, message databases, screenshots, QR codes, or credentials. Operators obtain official clients directly from Tencent.

CI runs tests, a reachable Go vulnerability scan, a secret scan, dependency-license checks, Skill validation, and a prohibited-payload check. Release builds publish only project-owned source-derived artifacts, checksums, and SBOMs. Verify checksums and provenance before installation.

## Reporting a vulnerability

Follow [SECURITY.md](../SECURITY.md). Do not attach real messages, account profiles, QR codes, database keys, verification codes, or official client packages to an issue or report. Reproduce with synthetic data whenever possible.
