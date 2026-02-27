# Security Policy

## Supported Versions

| Version | Supported |
| ------- | --------- |
| latest  | Yes       |

Only the latest release receives security updates. Users are encouraged to stay
on the most recent version.

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, use [GitHub Security Advisories](https://github.com/sydlexius/stillwater/security/advisories/new)
to report vulnerabilities privately. This ensures the issue can be triaged and
a fix prepared before public disclosure.

When reporting, please include:

- A description of the vulnerability and its potential impact
- Steps to reproduce or a proof-of-concept
- The version(s) affected
- Any suggested fix, if you have one

You should receive an initial response within 72 hours acknowledging receipt.
Once the issue is confirmed, a fix will be developed and released as soon as
practical, typically within 14 days for critical issues.

## Scope

The following are in scope:

- The Stillwater application binary and Docker image
- The REST API (`/api/v1/`)
- Authentication and session management
- Encryption of stored secrets (API keys)
- File system operations (NFO writes, image saves)

The following are out of scope:

- Vulnerabilities in upstream dependencies (report those to the upstream project)
- Issues requiring physical access to the host machine
- Denial of service through expected resource usage

## Security Measures

Stillwater implements the following security controls:

- **Encryption at rest:** API keys are encrypted with AES-256-GCM
- **Log scrubbing:** Sensitive values (API keys, passwords, tokens) are redacted
  from log output
- **CSRF protection:** All state-changing requests require a valid CSRF token
- **Input validation:** All user input is validated at the API boundary
- **No CGO:** The binary uses pure-Go SQLite (`modernc.org/sqlite`), eliminating
  a class of memory safety issues
