# Security policy

TopRelay makes apps on your computer reachable from the internet, so security bugs matter more here than in most
projects. Thank you for reporting them privately.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: the repository's **Security** tab → **Report a vulnerability**.
Please include what you found, how to reproduce it, and what an attacker could do with it. Don't open a public
issue for security bugs.

You'll get a reply within 3 working days. Once a fix ships, we'll credit you in the release notes unless you'd
rather not be named.

## What counts

The most serious bugs are ones that let someone:

- call a service without a valid remote-client credential (OAuth token or bearer key)
- call a tool the owner did not expose
- read or steal any credential: the agent key, bearer keys, OAuth tokens, the Cloudflare API token, or upstream
  credentials
- reach a local app, path or port that was not added as a service
- make the relay or agent log request bodies or secrets

## What TopRelay does not protect against

These are design limits, documented in the README, not vulnerabilities:

- **Cloudflare can see your traffic.** The relay ends TLS inside the Worker on your own Cloudflare account, so the
  traffic is not end-to-end encrypted between the bot and your app.
- **Bots you approve can do what you allowed.** A bot with a valid credential can call every tool you exposed to
  it.
- **`stdio` servers run as you.** A local MCP server started by the agent has your user's permissions.

## Supported versions

Until 1.0, only the latest release gets security fixes.
