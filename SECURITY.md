# Security

This project gives a paired device control over the Windows account that runs
`codex-mobile.exe`. Treat the access URL and token as passwords.

## Deployment rules

- Keep Funnel disabled unless public access is explicitly required.
- Use the same tailnet for one person's computer and phone; use separate
  tailnets for different deployments.
- Never publish `token.txt`, `tsnet/`, `sessions.json`, logs, QR images, signing
  keys, or the complete `%LOCALAPPDATA%\CodexMobile` directory.
- Run Codex with the least-privilege sandbox and approval policy that fits the
  task.
- Rotate the access token if a paired URL or QR image is exposed.

## Reporting

Do not open a public issue containing credentials, local paths, Tailscale node
names, private hostnames, or screenshots with personal data. Report the problem
privately to the repository owner.
