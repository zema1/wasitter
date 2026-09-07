# Security policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub's security advisory form](https://github.com/zema1/wasitter/security/advisories/new).
Include the affected version or commit, a minimal reproducer, the runtime and
grammar module involved, and the impact you observed. Do not open a public
issue for an unpatched vulnerability.

If private advisories are unavailable for your account, open a public issue
with only the text `security contact requested`; do not include exploit details
or sensitive input. A maintainer will provide a private channel.

## Scope

Reports involving guest-memory access, host crashes or panics, unexpected host
callbacks/imports, cancellation bypasses, or supply-chain problems in the
artifact build are in scope. A denial of service that requires an application
to accept unlimited untrusted input may still be a deployment concern rather
than a library vulnerability; include the smallest input and resource profile
you can provide so it can be assessed.

The project does not authenticate caller-supplied WebAssembly modules. Users
loading modules outside the checked-in assets are responsible for verifying
their origin and integrity.

## Supported versions

Security fixes target the latest release. If a vulnerability affects an older
release, upgrading is the recommended remediation unless a maintainer states
otherwise in the advisory.

