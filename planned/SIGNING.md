# Signing the binaries

Status: planned, not started.

Release binaries are not signed today. Windows SmartScreen warns about
`rimor.exe`, and macOS Gatekeeper blocks `rimor` until its quarantine
attribute is removed. The release notes say how to get past both.

## Windows: SignPath Foundation

[SignPath Foundation](https://signpath.org) signs open source projects for
free, through [SignPath.io](https://signpath.io). The conditions below come
from their [terms](https://signpath.org/terms), which were marked as a draft
when read (October 2026), and their
[GitHub integration docs](https://docs.signpath.io/trusted-build-systems/github).
Check both again before applying.

### Eligibility

| Requirement | rimor |
|---|---|
| OSI-approved license, no commercial dual licensing, no proprietary code | Yes: MIT, and every dependency is open source |
| Actively maintained, already released, documented on its download page | Yes |
| Built from the public source by automated CI that can be verified | Yes: GitHub Actions on GitHub-hosted runners |
| No malware or hacking tools; respects privacy; can be uninstalled | Yes: rimor's only request of its own is the daily update check, documented and optional; deleting the binary removes it |
| Sign only your own binaries | Yes: `rimor.exe` is built from this repository |

### What it asks of us

- **Manual approval of every release.** An Approver confirms each signing
  request on SignPath.io, so a Windows release waits for that click.
- **Multi-factor authentication** for every team member, on SignPath and on
  GitHub.
- **A "Code signing policy" section** on rimor.dev, linked from the
  download and release pages. It must say that code signing is provided
  free by SignPath.io with a certificate from SignPath Foundation. It must
  also list the team roles (Authors, who commit without further review;
  Reviewers; Approvers) with their members, and include a privacy statement.
- **Product name and version metadata** in the signed `.exe`. The build
  embeds no Windows version information today; one step adds it (for
  example with `go-winres`) from the release tag.

### What to accept up front

- **The publisher is SignPath Foundation,** not the project or its
  maintainers: the certificate is theirs. Windows shows "Verified publisher:
  SignPath Foundation".
- **The SmartScreen warning may not disappear at once.** SmartScreen also
  weighs how often a file has been downloaded. A signed file should earn
  that reputation faster than an unsigned one, but the first signed release
  may still warn.
- **SignPath Foundation can pause or end a subscription** and revoke
  certificates.

### How the release workflow changes

The Windows binaries stay cross-compiled on the Linux runner: what matters
is that every job leading up to the signing request runs on GitHub-hosted
runners. SignPath checks with GitHub that the file came from our workflow
run.

1. The Windows build jobs (amd64 and arm64) embed the version information
   and upload `rimor.exe` as a workflow artifact, unsigned.
2. A new job submits it with `signpath/github-action-submit-signing-request`
   (v3 or later), passing the upload step's `artifact-id` as
   `github-artifact-id`, and waits for the approval.
3. It downloads the signed `.exe`, packs the zip and its checksum, and the
   release continues as today. Linux and macOS are unaffected.

Notes for the job:

- `actions/upload-artifact` makes a zip, so SignPath's artifact
  configuration must have `<zip-file>` at its root. Alternatively, upload
  with `archive: false` and set `skip-decompress: true` to sign the `.exe`
  directly.
- The job needs a `SIGNPATH_API_TOKEN` secret (for a user with submitter
  permission on the project and signing policy), plus the SignPath
  organisation and project IDs. `GITHUB_TOKEN` needs `actions: read` and
  `contents: read`.
- Until the secret exists, the job is skipped and releases stay unsigned,
  so the workflow can be merged before the application is accepted.
- The SignPath Trusted Build System for GitHub.com must be added to the
  SignPath organisation and linked to the project.

### Steps

1. Apply to SignPath Foundation (the form on signpath.org). Once accepted,
   create the project, the signing policy and the API token, and turn on
   MFA everywhere.
2. Embed the Windows version information in the build.
3. Add the code signing policy section to rimor.dev and link it from the
   release notes.
4. Add the signing job to `release.yml`, skipped without the secret.
5. Update the release notes and README, which today explain how to get
   past SmartScreen.

## macOS

Not covered by SignPath Foundation. Signing and notarization need an Apple
Developer account ($99 a year) and `codesign` and `notarytool`, which run on
a macOS runner. Without them, a Homebrew formula in our own tap could
avoid the warning, since Homebrew should not quarantine what a formula
installs (unlike a cask). Check that before relying on it.
