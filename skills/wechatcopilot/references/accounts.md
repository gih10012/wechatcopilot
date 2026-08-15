# Accounts and authentication

## Host storage gate

Before any real-account login, require `doctor` to report `swap_confidentiality` healthy. When a dedicated or file-backed state mount is configured, also require the `state_mount` check to be present and healthy. Accept no swap, a real zram block device whose sysfs `backing_dev` is absent or `none`, or a separately verified dm-crypt swap device; the provisioning script itself only warns, while the daemon refuses to start or restore accounts when the swap check fails.

When the backing disk is NTFS3, use it only for the outer fully allocated 64 GiB LUKS2 image. The live state root is the inner ext4 mount. Keep the outer filesystem UUID distinct from the inner ext4 UUID: every mutating provisioning command (`create`, `configure`, `unlock`, and `lock`) requires the outer UUID, while the daemon's three mount-gate variables identify the inner mapper, ext4 type, and ext4 UUID.

Keep provisioning and unlock in a trusted interactive terminal. `create` closes the manually formatted mapper and asks for the passphrase again while systemd reopens it. After changing `/share` idle-timeout settings, run `systemctl daemon-reload`. Before `daemon install`, export `WECHATCOPILOT_HOME` and all three gate variables; the installer writes the three constraints to its required `state-mount.environment` and refuses a later implicit downgrade when those variables are missing. Do not configure TPM or key-file automatic unlock. Before `lock`, stop the daemon and all project containers.

## Add and authenticate

1. Run `doctor` and resolve all blocking checks.
   A failed `state_mount` check means the pinned encrypted state volume is absent or wrong; do not start the daemon or create a replacement directory at that path.
2. Add an account with a unique human alias; record the returned opaque `account_id`.
3. Activate the account.
4. Run `accounts login` in a trusted terminal. Add `--lan` only when the user needs to complete login from another device on the same private network. Let the daemon prefer the default-route interface, or add `--lan-address <RFC1918_IP>` only for an exact address assigned to another eligible local interface.
5. Open the one-time URL directly, complete QR scan, phone confirmation, or SMS entry, and wait for `ONLINE`.
6. Verify that restarting the runtime preserves `ONLINE` before relying on unattended reads.

Login challenges expire after ten minutes and are single-use. A LAN challenge serves no external scripts and rejects public, wildcard, loopback, container-bridge, and unassigned binding addresses. `--lan-address` requires `--lan`; `WECHATCOPILOT_LAN_ADDRESS` is only considered for an explicitly requested LAN challenge. After success the challenge retains the completion result for about 60 seconds, then closes; an expired challenge closes without authenticating.

## Switch accounts

Only one saved account for each platform can run at a time. Activate the requested account explicitly. The daemon first stops and flushes the previous account before mounting the new profile. WeChat and WeCom use independent platform slots and can run concurrently.

Inactive accounts retain their profiles and local index. Results from them are stale by definition; surface actions and sends require the account to be active and online.

## Remove an account

Removal is always permanent in v0.1. It unregisters the saved account and deletes its local profile, runtime state, and indexed content. First verify the opaque account ID, deactivate it, explain the deletion, and require both `--confirm` and `--purge`. Never translate a request to "log out" into account removal.

The daemon persists `deleting:true` before driver cleanup begins. A deleting account remains visible in `accounts list` but fails closed for status, activation, reads, sends, and surfaces. If removal returns a retryable `CONFLICT`, do not try to recover or reactivate the account; repeat the exact removal command with the same account ID, `--confirm`, and `--purge`. The marker survives daemon restarts until cleanup completes.

## Recover authentication

When Tencent invalidates a session, preserve the profile and enter `AUTH_REQUIRED`. Create a new login challenge rather than adding a replacement account. Stop if the official client reports a risk warning, device rejection, or unsupported environment; the project does not automate around these controls.
