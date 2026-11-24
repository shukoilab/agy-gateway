# Security Policy

## 1. Supported Versions

Security updates, vulnerability remediation, and protocol patches are actively maintained for the following versions:

| Version | Supported          | Status |
|:-------:|:------------------:|:------:|
| `3.9.x` | :white_check_mark: | Current Active Stable Release |
| `3.8.x` | :white_check_mark: | Critical Security Patches Only |
| `< 3.8` | :x:                | End of Life (Deprecated) |

---

## 2. Reporting a Vulnerability

We take the security of **Antigravity Gateway (`agy-gateway`)** seriously. To prevent premature public disclosure that could compromise systems before a remediating patch is released, **DO NOT report suspected security vulnerabilities through public GitHub Issues, Pull Requests, or Discussions.**

### Coordinated Vulnerability Disclosure (CVD) Process

1. **Private Reporting via GitHub Security Advisories:**
   - Navigate to the repository's **Security** tab.
   - Click **Report a vulnerability** to open a private Coordinated Vulnerability Disclosure draft.
   - Alternatively, contact the maintainers directly through the repository's designated private security contact channel on GitHub.

2. **Information to Include in Your Report:**
   To help our research team isolate and remediate the issue rapidly, please provide:
   - **Vulnerability Description:** Detailed explanation of the vulnerability, attack vector, and potential impact.
   - **Environment Details:** Target architecture (`amd64`, `arm64`), Go runtime version, Docker image digest, and active configuration flags.
   - **Proof of Concept (PoC):** Minimal, standalone script, payload, or network capture demonstrating deterministic reproduction.
   - **Suggested Remediation (Optional):** If you have identified a potential fix, architectural patch, or boundary mitigation.

3. **Response Timelines & Triage:**
   - **Acknowledgment:** Within 48 hours of initial submission.
   - **Triage & Assessment:** Within 5 business days, evaluating severity, CVSS vector, and affected components.
   - **Patch Development:** Work will occur in a private repository fork.
   - **Public Release & Advisory:** A patch release and coordinated CVE/GHSA advisory will be published simultaneously once the fix is validated.

---

## 3. Security Architecture & Threat Model

Antigravity Gateway implements defense-in-depth principles across all ingress and internal boundaries:

### 3.1 Input Hardening & Zero-Trust Boundary
- Strict request size limits (`max_request_body_bytes`, default: 100 MiB) to guard against resource exhaustion and memory flooding.
- Constant-time comparison (`subtle.ConstantTimeCompare`) for all API token and secret validations to mitigate side-channel timing attacks.
- Strict JSON schema decoding with boundary validation on all consumer payloads.

### 3.2 Credential & Token Isolation
- OAuth credentials, refresh tokens, and authentication cookies are encrypted or stored exclusively inside the isolated vault XDG directory (`/var/lib/agy-gateway/vault-xdg`).
- Sensitive credential export is disabled by default (`allow_credential_export: false`).
- Session tokens are never echoed in logs or transmitted across untrusted network perimeters.

### 3.3 Process & Workspace Sandboxing
- Worker subprocesses execute in isolated scratch directories (`/var/lib/agy-gateway/workers/worker_*`).
- Ephemeral SQLite session databases and internal brain artifacts are comprehensively purged upon session closure to prevent multi-tenant data leakage.
- Process group handles (`pgid`) are tracked with automated teardown on `SIGINT`/`SIGTERM` to prevent orphaned child workers.
