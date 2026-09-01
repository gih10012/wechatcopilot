# Accounts and authentication

## Host storage gate

Before any real-account login, inspect `doctor`. When a dedicated or file-backed state mount is configured, require the `state_mount` check to be present and healthy. A `swap_confidentiality` warning records that raw swap may retain decrypted account data; report it, but never disable or reconfigure host swap through an agent. Stop when that check has `ok:false`; either the operator enabled strict policy and an active target is unprotected, or `WECHATCOPILOT_STRICT_SWAP` is invalid. Strict mode accepts no swap, a real zram block device whose sysfs `backing_dev` is absent or `none`, or a separately verified dm-crypt swap device.

When the backing disk is NTFS3, use it only for the outer fully allocated 64 GiB LUKS2 image. The live state root is the inner ext4 mount. Keep the outer filesystem UUID distinct from the inner ext4 UUID: every mutating provisioning command (`create`, `configure`, `unlock`, and `lock`) requires the outer UUID, while the daemon's three mount-gate variables identify the inner mapper, ext4 type, and ext4 UUID.

Keep provisioning and unlock in a trusted interactive terminal. `create` closes the manually formatted mapper and asks for the passphrase again while systemd reopens it. After changing `/share` idle-timeout settings, run `systemctl daemon-reload`. Before `daemon install`, export `WECHATCOPILOT_HOME` and all three gate variables; the installer writes the three constraints to its required `state-mount.environment` and refuses a later implicit downgrade when those variables are missing. Do not configure TPM or key-file automatic unlock. Before `lock`, stop the daemon and all project containers.

## Add and authenticate

1. Run `doctor` and resolve all blocking checks.
   A failed `state_mount` check means the pinned encrypted state volume is absent or wrong; do not start the daemon or create a replacement directory at that path.
2. Add an account with a unique human alias; record the returned opaque `account_id`.
3. Activate the account.
4. Stop agent execution and tell the user to run `accounts login` manually in a separate trusted terminal. Never invoke it or any `auth` subcommand through an agent shell or tool, and never ask the user to paste their inputs or outputs: those commands handle a bearer login URL or verification secret. Add `--lan` only when the user needs to complete login from another device on the same private network. Let the daemon prefer the default-route interface, or add `--lan-address <RFC1918_IP>` only for an exact address assigned to another eligible local interface.
5. Open the one-time URL directly, complete QR scan, phone confirmation, or SMS entry, and wait for stable `ONLINE`. A saved personal-WeChat profile may first offer both “登录当前微信账号” and “切换登录方式”; only the user may confirm either image-bound action. Use the latter when Tencent rejects or rolls back the saved-account quick login, then complete the official client's refreshed QR or phone flow.
6. Verify that restarting the runtime preserves `ONLINE` before relying on unattended reads.

Login challenges expire after ten minutes and are single-use. A LAN challenge serves no external scripts and rejects public, wildcard, loopback, container-bridge, and unassigned binding addresses. `--lan-address` requires `--lan`; `WECHATCOPILOT_LAN_ADDRESS` is only considered for an explicitly requested LAN challenge. A transient main window is not success: the official client must remain `ONLINE` across multiple observations for at least 15 seconds. After stable success the challenge retains the completion result for about 60 seconds, then closes; an expired challenge closes without authenticating.

## Switch accounts

Only one saved account for each platform can run at a time. Activate the requested account explicitly. The daemon first stops and flushes the previous account before mounting the new profile. WeChat and WeCom use independent platform slots and can run concurrently.

Inactive accounts retain their profiles and local index. Results from them are stale by definition; surface actions and sends require the account to be active and online.

## Remove an account

Removal is always permanent in v0.1. It unregisters the saved account and deletes its local profile, runtime state, and indexed content. First verify the opaque account ID, deactivate it, explain the deletion, and require both `--confirm` and `--purge`. Never translate a request to "log out" into account removal.

The daemon persists `deleting:true` before driver cleanup begins. A deleting account remains visible in `accounts list` but fails closed for status, activation, reads, sends, and surfaces. If removal returns a retryable `CONFLICT`, do not try to recover or reactivate the account; repeat the exact removal command with the same account ID, `--confirm`, and `--purge`. The marker survives daemon restarts until cleanup completes.

## Adopt a legacy message index

Current indexes carry their owning account UUID. A recognized row-empty legacy index can be bound automatically, but a non-empty index without ownership metadata requires an explicit offline migration. After making a state backup, stop the daemon and run this operator-only command with the exact registered account ID or alias:

```bash
wechatcopilot accounts adopt-legacy-index --account ACCOUNT_ID --confirm --json
```

The command holds the same state lock as `daemon serve`, so it returns `CONFLICT` while any daemon owns that state home. It uses `WECHATCOPILOT_HOME` or `--home`, enforces the configured state-mount gate, resolves the account from that registry, and adopts only the existing `accounts/ACCOUNT_UUID/index.sqlite3`. It refuses missing or linked account state, a missing index, an empty or already-owned database, and any unrecognized schema or failed integrity check. It creates no replacement account directory or index and prints no indexed messages. Restart the daemon only after a successful result.

## Approve a stopped legacy WeCom profile

If a registered WeCom account already has non-empty Android `/data` but lacks external profile metadata, stop the daemon and obtain explicit operator confirmation before running:

```bash
wechatcopilot accounts approve-legacy-wecom-profile --account ACCOUNT_ID --confirm --json
```

The command succeeds only while the exact digest-pinned account container is stopped and has the expected isolated network and bind mount; it brackets durable approval publication with the same immutable container ID, complete stopped execution epoch, and pinned canonical data path/inode. It creates no container, profile marker, or replacement directory. The persistent approval excludes `st_dev` so a verified dm-crypt remount does not invalidate it, while each publication and consumption frame still compares the live device/inode. The one-use approval is consumed before stopped-profile migration writes markers; running or changing the container first invalidates and revokes it. Do not infer this confirmation from activation or another account operation, and do not retry a failed approval without reporting why its exact stopped-container or filesystem frame failed.

## Recover authentication

When Tencent invalidates a session, preserve the profile and enter `AUTH_REQUIRED`. Ask the user to create a new login challenge manually rather than adding a replacement account; never create it through an agent tool. If the saved-account quick login returns to its initial screen, tell the user they may explicitly confirm “切换登录方式” on that same trusted page; this is a fixed, screenshot-bound route to the official client's other login flow, not permission for the agent to click it. Stop if the official client reports a risk warning, device rejection, or unsupported environment; the project does not automate around these controls.
