# Contributing

`wechatcopilot` operates personal communication accounts. Favor narrow semantic interfaces, deterministic failure, and synthetic fixtures over convenience features that widen control or data exposure.

## Before proposing a change

Open an issue for a new driver, authentication mechanism, persistent data source, network listener, or externally visible write behavior. Describe its account-risk, data-flow, and licensing implications.

The following are intentionally out of scope and will not be accepted:

- Private-protocol emulation, credential migration, anti-detection, Root hiding, TLS interception, or client patching.
- Cloud-device dependencies or sending account data to a hosted relay.
- Arbitrary shell, SQL, X11 coordinates/input, ADB, browser script, or VNC APIs exposed to agents.
- Payment, transfer, mass messaging, automatic group sending, or security-setting automation.
- Official WeChat/WeCom packages, logged-in profiles, keys, QR codes, screenshots, or real messages in source, fixtures, issues, CI, images, or releases.

## Development setup

Use the Go version declared in `go.mod` and the checked-in Gradle wrapper for the Android companion. Build and test with:

```bash
make check
make test
```

The default tests use fake drivers plus synthetic SQLite, notification, accessibility-tree, and command-runner fixtures. They do not start a real client or require an account. Run real-client tests only on an isolated local host and never in a public CI job.

Validate the bundled Skill with:

```bash
python3 scripts/validate_skill.py skills/wechatcopilot
```

Maintainers should additionally use `quick_validate.py` from the Codex `skill-creator` package when it is installed.

## Pull requests

- Keep public CLI, MCP, and driver semantics synchronized and document user-visible changes.
- Add tests for lifecycle transitions, cross-account isolation, idempotency, partial reads, and uncertain sends where relevant.
- Preserve provenance, completeness, confidence, and stable error codes across adapters.
- Add or update dependency license metadata. Do not copy source from projects without a compatible license.
- Run the secret and prohibited-payload checks before pushing.
- Use synthetic identities, messages, paths, screenshots, QR codes, and databases in all examples.

Do not include generated account state or official-client artifacts in a pull request, even when Git ignores them. If accidental sensitive data reaches Git history, stop and follow the private security-report process instead of merely deleting the latest copy.

## Driver compatibility changes

Record the exact official-client version and observation mechanism affected. A compatibility claim requires repeatable read-only, restart-persistence, send-verification, and failure-state evidence. WeCom emulator rejection or account-risk warnings are hard stops; contributors must not work around them.

## Licensing

Contributions are accepted under the repository's MIT license. By submitting a contribution, you confirm that you have the right to provide it under that license and that any third-party material is identified in `THIRD_PARTY_NOTICES.md` and `LICENSES/` as required.

Maintainers should follow [docs/releasing.md](docs/releasing.md) for signing, release gates, and artifact verification.
