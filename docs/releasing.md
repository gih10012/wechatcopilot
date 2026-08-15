# Releasing

Only maintainers should create a release tag. A release is source-derived and must never contain an official Tencent client, account profile, message database, screenshot, QR code, verification material, or local runtime volume.

## Signing setup

Create a dedicated Android release keystore offline. Do not reuse a personal or unrelated application key. Store one encrypted backup outside GitHub and configure these repository Actions secrets:

- `ANDROID_RELEASE_KEYSTORE_B64`: base64 of the complete keystore file.
- `ANDROID_RELEASE_KEYSTORE_PASSWORD`: keystore password.
- `ANDROID_RELEASE_KEY_ALIAS`: release key alias.
- `ANDROID_RELEASE_KEY_PASSWORD`: key password.

The workflow decodes the keystore only below the runner's temporary directory, signs the project-owned companion, verifies its certificate, and deletes the temporary file. A tag build fails closed when any signing secret is absent.

## Release gate

Before tagging, require a green `main` CI run and complete the documented real-client acceptance checks on a private host. In particular, verify login persistence, exact-target resolution, one confirmed send to a controlled destination, uncertain-send handling, and the capability map for both client versions. WeCom must have passed the unmodified official-client environment gate without an account-risk warning.

Create an annotated `vMAJOR.MINOR.PATCH` tag only from the reviewed `main` commit. The release workflow repeats Go tests, Skill and payload checks, dependency-license checks, Android tests, Android Licensee, and the companion build.

## Published artifacts

The workflow publishes:

- Linux amd64 and arm64 `wechatcopilot` archives.
- A signed project-owned Android companion APK.
- SPDX JSON SBOM, SHA-256 checksums, and GitHub build-provenance attestations.
- MIT and third-party dependency notices inside each Go archive.

The workflow constructs archives from an explicit allowlist and rejects an unexpected filename or archive member. It does not build or publish an image containing WeChat or WeCom. Runtime base images and official clients remain operator-side inputs pinned by digest or SHA-256.

After publication, download the release into a clean directory, verify checksums and attestations, inspect the APK signing certificate, and perform a fresh install smoke test before announcing compatibility.
