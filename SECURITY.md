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

## What TopRelay gives apps

- **`X-TopRelay-Caller`**: an HMAC the agent adds to every relayed request, over the method, path, query, client,
  time and body, keyed per service (`toprelay caller-key`). It proves the request came through TopRelay; the
  relay never forwards a copy from a bot, and the agent removes one if it arrives. Verifiers: `MIGRATION.md`.
- **Credentials off the command line**: `--header-file` (read on every request) and `--header-stdin` (keyring).
  `--header` still works for compatibility and warns.
- **Port-squatting defenses**: Unix socket upstreams with owner, mode and peer-user checks; on macOS, an optional
  code-signature pin for TCP upstreams. Limits are in `agent/README.md`.

- **Pinned tool definitions**: a tool whose description, schema or annotations change after approval is held
  (hidden and refused) until the owner approves the change (`toprelay pins`).
- **Answer shield**: credentials in what an app returns are redacted (or the answer withheld) on the computer,
  before the relay sees them; instruction-like text is flagged (`toprelay shield`).
- **Tamper-evident call log**: hash-chained lines with Ed25519-signed checkpoints (`toprelay audit verify`).

## What TopRelay does not protect against

These are design limits, documented in the README, not vulnerabilities:

- **Cloudflare can see your traffic.** The relay ends TLS inside the Worker on your own Cloudflare account, so the
  traffic is not end-to-end encrypted between the bot and your app.
- **Bots you approve can do what you allowed.** A bot with a valid credential can call every tool you exposed to
  it.
- **`stdio` servers run as you.** A local MCP server started by the agent has your user's permissions.
- **Pattern-based inspection.** The shield matches known credential formats and known injection phrasing. A
  secret with no recognizable shape (unless TopRelay itself holds it), or an instruction phrased in a new way, gets
  through. Injection findings are warnings: they never change an answer.
- **Other programs running as you.** A process with your user's rights can read your keyring entries and files
  (including the caller key and the pins file) and can send requests to your apps directly. They can also derive
the call-log signing key, so the audit trail proves integrity against everyone *else*. TopRelay's checks keep *other users* and
  the network out; they don't sandbox your own account.

## Supported versions

Until 1.0, only the latest release gets security fixes.
