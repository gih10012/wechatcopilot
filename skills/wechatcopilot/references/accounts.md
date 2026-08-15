# Accounts and authentication

## Add and authenticate

1. Run `doctor` and resolve all blocking checks.
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
