# Third-party notices

This repository does not include or redistribute the official WeChat Linux client, official WeCom Android client, Tencent account data, or third-party protocol-server binaries. Operators obtain official clients directly from Tencent under Tencent's terms.

The project currently links or builds against the following direct open-source dependencies. Their source repositories and distribution artifacts contain the authoritative copyright notices and license texts.

| Component | Purpose | License |
| --- | --- | --- |
| `github.com/modelcontextprotocol/go-sdk` | MCP server implementation | Apache-2.0 AND MIT |
| `github.com/skip2/go-qrcode` | Login-link QR rendering | MIT |
| `github.com/spf13/cobra` | CLI command tree | Apache-2.0 |
| `golang.org/x/sys` | Linux system-call integration | BSD-3-Clause |
| `golang.org/x/term` | Private terminal verification-code input | BSD-3-Clause |
| `modernc.org/sqlite` | Pure-Go SQLite message index | BSD-3-Clause |
| Kotlin standard library | Android companion runtime | Apache-2.0 |
| Android Gradle Plugin | Android companion build | Apache-2.0 |
| Gradle | Android companion build | Apache-2.0 |
| CashApp Licensee | Android dependency-license verification | Apache-2.0 |

Android/Gradle dependencies, container base packages, and transitive Go dependencies are resolved from their upstream sources at build time. Generated SBOMs enumerate the exact dependency set for each release. `LICENSES/allowed.txt` defines the automated source-dependency license policy; it is not a replacement for upstream notices.

The project was informed by public documentation and behavior of other WeChat automation projects, but no source code from Webox, MimicWX, WeChatPadPro, or other protocol/automation projects is copied or derived here.
