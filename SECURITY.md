# Security policy

## Supported versions

Until the first stable release, only the latest commit on `main` receives security fixes. After releases begin, the newest minor release and `main` will be supported. Compatibility with a particular official-client version is reported separately and is not implied by the project version.

## Report privately

Do not open a public issue for a vulnerability that could expose an account, message, login profile, authentication challenge, database key, or remote-control surface.

Use GitHub's **Security > Report a vulnerability** private advisory form for this repository. Include the affected commit or release, platform, driver, client version, impact, and synthetic reproduction steps. If private advisories are temporarily unavailable, open a public issue containing no vulnerability detail and ask the maintainer to establish a private channel.

Never attach real account state, QR codes, verification codes, message databases, screenshots, official Tencent packages, private conversation content, or login URLs. Replace identifiers and messages with synthetic data.

## Response targets

- Initial acknowledgement: within 5 business days.
- Triage and severity decision: within 10 business days.
- Remediation timeline: communicated after reproduction and scope are understood.

These are targets, not a service-level agreement. The project may immediately disable or mark a driver version incompatible when continued use could put accounts at risk.

## Scope

Security issues include authentication challenge exposure, cross-account state access, unauthorized or duplicate sends, daemon socket authorization bypass, secret logging, public debug ports, prohibited data in release artifacts, and unsafe driver actions.

Tencent account-policy enforcement, official-client vulnerabilities, and availability of WeChat or WeCom are outside this project's control. Report official-client vulnerabilities to Tencent through its security process as well as notifying this project when our integration increases the impact.
