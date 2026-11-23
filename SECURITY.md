# Security Policy

## Supported Versions

| Version | Supported |
| --- | --- |
| `0.1.x` | Yes |

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report vulnerabilities privately using **GitHub Security Advisories** (**Security -> Advisories -> Report a vulnerability** on the repository).

When reporting an issue, please include:
- A clear description of the vulnerability and affected endpoints or components
- Steps to reproduce or a proof-of-concept request
- Any suggested remediation if known

We will acknowledge reports promptly and coordinate a fix and advisory before public disclosure.

## Security Architecture Notes

MergerFin is designed with credential isolation and Jellyfin's native permission model in mind:

- **Admin-Only Management Endpoints**: All plugin management routes (`/MergerFin/Servers/Test`, `/MergerFin/Sync`, `/MergerFin/Status`) enforce Jellyfin's `[Authorize(Policy = "RequiresElevation")]` policy.
- **API Key Isolation in `proxy` Mode**: When a remote server is configured in `proxy` mode (the default), MergerFin streams media server-side and injects the remote `ApiKey` / `X-Emby-Token` only on upstream requests so remote API keys are never exposed to clients.
- **Mesh Loop Prevention**: Relayed sessions and remote `.strm` / `mf_*` sources are filtered out during catalog synchronization and playback reporting to prevent feedback loops between federated servers.
