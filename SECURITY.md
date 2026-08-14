# Security Policy

## Supported versions

Only the latest release is supported with security fixes. Please upgrade to the
newest version to receive updates.

## Reporting a vulnerability

Please **do not** open a public issue for security vulnerabilities. Instead,
report them privately via GitHub's private vulnerability reporting feature
(Repository → Security → Report a vulnerability), or by contacting the
maintainers directly.

Please include:

- A description of the vulnerability and its impact
- Steps to reproduce it
- Any relevant configuration or environment details

We aim to acknowledge reports within 5 business days and to keep you updated
on progress.

## Security notes

- `SECRET_KEY` is used to encrypt stored user passwords (AES-GCM). **Always**
  set it to a long, random value in production — the default is for development
  only.
- StreamMux fetches remote URLs from configured addons. Only add addons you
  trust, and be aware of the content they resolve.
- The mux endpoint streams content directly; ensure the instance is only
  reachable by intended users (e.g. behind a reverse proxy with auth) if it is
  publicly exposed.
