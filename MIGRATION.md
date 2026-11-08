# Migrating an app to TopRelay 0.2

For authors of local apps (MCP servers, HTTP services) that TopRelay exposes to cloud bots. Every change here is
backwards compatible: a 0.1 integration keeps working, but it is less safe than it needs to be.

The `--json` contract is unchanged: every command prints exactly one JSON object on stdout and never prompts.
New fields may appear in objects; existing ones keep their meaning.

## 1. Stop passing your token on the command line

`toprelay add … --header "X-App-Token: $TOKEN"` puts the token in `argv`, where any process can read it (`ps`).
Pick one of these instead.

**Best: point TopRelay at the file your app already keeps.** The agent reads it on every request, so you can
rotate the token by rewriting the file, and TopRelay never needs to be told.

```sh
toprelay add myapp --mcp-url http://127.0.0.1:7879/mcp \
  --header-file "X-App-Token=$HOME/Library/Application Support/MyApp/mcp-token" --json
```

The file must be an absolute path to a regular file owned by the user running the agent, with no group or other
permissions (`chmod 600`), at most 8 KiB. Surrounding whitespace is trimmed. Write it atomically (write a temp
file, then rename it over the old one) so a request never reads half a token. If the file is missing or has the
wrong mode, the bot gets "TopRelay can't get the credential for the local app" and your app sees nothing: the
agent never sends a request without its credential.

**Or: stdin.** The value goes to the OS keyring.

```sh
printf 'X-App-Token: %s\n' "$TOKEN" | toprelay add myapp --mcp-url http://127.0.0.1:7879/mcp --header-stdin --json
```

From Swift, write to the process's stdin pipe rather than building an argument:

```swift
let p = Process()
p.executableURL = toprelayURL
p.arguments = ["add", "myapp", "--mcp-url", localURL, "--header-stdin", "--tools-default", "allow-readonly", "--json"]
let stdin = Pipe()
p.standardInput = stdin
try p.run()
stdin.fileHandleForWriting.write(Data("X-App-Token: \(token)\n".utf8))
try stdin.fileHandleForWriting.close()
```

### Rotating the token: `set-header`, not remove + add

`remove` followed by `add` redeploys the relay and revokes every OAuth grant, so claude.ai users have to pair again.
To change only the token:

```sh
printf 'X-App-Token: %s\n' "$NEW" | toprelay set-header myapp --stdin --json
# {"service":"myapp","header":"X-App-Token","updated":true,"source":"keyring"}
toprelay set-header myapp --file "X-App-Token=/path/to/token" --json    # switch to a file
toprelay set-header myapp --clear --json                                 # send no credential
```

A running `toprelay up` applies it within 2 seconds, on the same connection. Nothing is redeployed; bearer keys and
OAuth grants keep working. With `--header-file` you don't need even this: rewrite the file.

Replace "remove if it exists, then add" with:

```text
list --json → service exists?  yes → set-header <name> --file … (or --stdin)
                               no  → add <name> --mcp-url … --header-file …
```

## 2. Know for sure that a call is remote: verify `X-TopRelay-Caller`

Apps often give bots fewer powers than local tools (no file paths, no deletes). Guessing from
`X-Forwarded-For`, `CF-Connecting-IP` or `X-Toprelay-Client` is weak: any local process can send those.

Every request the agent relays now carries a header only the agent can make:

```
X-TopRelay-Caller: v1; client=https://claude.ai/oauth/…; ts=1793500000; nonce=9f…; sig=d424…
```

`sig` is an HMAC-SHA256 over the method, the path and query, the client id, the time, a nonce and the SHA-256 of
the body, keyed with the service's **caller key**. Get the key:

```sh
toprelay caller-key myapp --json
# {"service":"myapp","header":"X-TopRelay-Caller","key":"…43 chars…","path":"~/.config/toprelay/caller-keys/myapp.key","source":"toprelay","rotated":false}
```

Read it from `path` (mode 0600) when your app starts, or whenever verification fails (the user may have run
`caller-key --rotate`). A sandboxed app that can't read `~/.config/toprelay` can supply its own key instead: write
at least 32 random characters to a 0600 file in its container and run
`toprelay caller-key myapp --file /path/in/container/caller.key`. Rotate it by rewriting the file.

**How to use the result.** Decide in this order:

1. A valid `X-TopRelay-Caller`: the call came through TopRelay from the bot named in `client` (an OAuth client id
   such as claude.ai's, or `key:<id>` for a bearer key). Apply your remote rules, per client if you like.
2. No valid signature, but `X-Forwarded-For`, `CF-Connecting-IP` or `X-Toprelay-Client` present: treat it as
   remote anyway. A local process gains nothing by pretending to be remote.
3. Neither: local.

What the signature adds over the old headers: the remote client's identity can't be faked, so per-client rules are
safe; and you can give TopRelay **its own token** that your app accepts only together with a valid signature. Then a
process that steals that token (for example by squatting your port, see section 3) still can't use it on its own,
and can replay a signed request only unchanged and within 60 seconds. To rule out replay entirely, remember each
`nonce` for 2 minutes and refuse repeats.

The verifiers below are 15-30 lines each, use only the standard library, and are tested against the
[protocol test vector](docs/protocol.md#test-vectors) on every `go test` (copies in `docs/verifiers/`). Each takes
the key's UTF-8 bytes, the header value, the method, the **raw request target** (path and query exactly as on the
request line), the body bytes, and returns the client id or nothing.

### Swift

```swift
import CryptoKit
import Foundation

/// Returns the caller's client id if the header is valid, else nil. `target` is the raw request path + query.
func verifyTopRelayCaller(key: Data, header: String, method: String, target: String, body: Data, now: Date = Date()) -> String? {
    let parts = header.split(separator: ";").map { $0.trimmingCharacters(in: .whitespaces) }
    var f: [String: String] = [:]
    for p in parts.dropFirst() { if let eq = p.firstIndex(of: "=") { f[String(p[..<eq])] = String(p[p.index(after: eq)...]) } }
    guard parts.first == "v1", let c = f["client"], let n = f["nonce"], let sig = f["sig"], let tsS = f["ts"], !tsS.isEmpty,
          tsS.allSatisfy({ $0.isASCII && $0.isNumber }), let ts = Double(tsS), abs(now.timeIntervalSince1970 - ts) <= 60,
          sig.count == 64, let sigBytes = Data(hexString: sig) else { return nil }
    let bodyHash = SHA256.hash(data: body).map { String(format: "%02x", $0) }.joined()
    let msg = ["toprelay-caller-v1", method, target, c, tsS, n, bodyHash].joined(separator: "\n")
    guard HMAC<SHA256>.isValidAuthenticationCode(sigBytes, authenticating: Data(msg.utf8), using: SymmetricKey(data: key)) else { return nil }
    return c.removingPercentEncoding
}

extension Data {
    init?(hexString s: String) {
        var out = Data(capacity: s.count / 2), i = s.startIndex
        while i < s.endIndex {
            let j = s.index(i, offsetBy: 2, limitedBy: s.endIndex) ?? s.endIndex
            guard let b = UInt8(s[i..<j], radix: 16), j > s.index(after: i) else { return nil }
            out.append(b); i = j
        }
        self = out
    }
}
```

Call it with the bytes your server read, before parsing the body:

```swift
let remoteClient = req.headers["x-toprelay-caller"].flatMap {
    verifyTopRelayCaller(key: callerKey, header: $0, method: req.method, target: req.target, body: req.body)
}
let isRemote = remoteClient != nil || req.headers["x-forwarded-for"] != nil || req.headers["x-toprelay-client"] != nil
```

### Go

```go
import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// VerifyTopRelayCaller returns the caller's client id and true if the header is valid. target is r.RequestURI.
func VerifyTopRelayCaller(key []byte, header, method, target string, body []byte, now time.Time) (string, bool) {
	parts := strings.Split(header, ";")
	f := map[string]string{}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			f[k] = v
		}
	}
	ts, err := strconv.ParseInt(f["ts"], 10, 64)
	if strings.TrimSpace(parts[0]) != "v1" || err != nil || strings.Trim(f["ts"], "0123456789") != "" || now.Sub(time.Unix(ts, 0)).Abs() > time.Minute {
		return "", false
	}
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.Join([]string{"toprelay-caller-v1", method, target, f["client"], f["ts"], f["nonce"], hex.EncodeToString(sum[:])}, "\n")))
	if !hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(f["sig"])) {
		return "", false
	}
	client, err := url.PathUnescape(f["client"])
	return client, err == nil
}
```

Use `r.RequestURI` as the target, and read the body before verifying (then restore it with
`r.Body = io.NopCloser(bytes.NewReader(body))`).

### Python

```python
import hashlib, hmac, time, urllib.parse

def verify_toprelay_caller(key: bytes, header: str, method: str, target: str, body: bytes, now=None):
    """Returns the caller's client id if the header is valid, else None. target is the raw request path + query."""
    parts = [p.strip() for p in (header or "").split(";")]
    f = dict(p.split("=", 1) for p in parts[1:] if "=" in p)
    if parts[0] != "v1" or not all(k in f for k in ("client", "ts", "nonce", "sig")) or not f["ts"].isdigit():
        return None
    if abs((time.time() if now is None else now) - int(f["ts"])) > 60:
        return None
    msg = "\n".join(["toprelay-caller-v1", method, target, f["client"], f["ts"], f["nonce"], hashlib.sha256(body).hexdigest()])
    if not hmac.compare_digest(hmac.new(key, msg.encode(), hashlib.sha256).hexdigest(), f["sig"]):
        return None
    return urllib.parse.unquote(f["client"])
```

With `http.server`, the target is `self.path`.

### Node

```js
import { createHash, createHmac, timingSafeEqual } from "node:crypto";

/** Returns the caller's client id if the header is valid, else null. target is the raw request path + query (req.url). */
export function verifyTopRelayCaller(key, header, method, target, body, now = Date.now() / 1000) {
  const [v, ...rest] = String(header ?? "").split(";").map((p) => p.trim());
  const f = Object.fromEntries(rest.filter((p) => p.includes("=")).map((p) => [p.slice(0, p.indexOf("=")), p.slice(p.indexOf("=") + 1)]));
  if (v !== "v1" || !/^\d+$/.test(f.ts ?? "") || [f.client, f.nonce, f.sig].includes(undefined)) return null;
  if (Math.abs(now - Number(f.ts)) > 60) return null;
  const bodyHash = createHash("sha256").update(body).digest("hex");
  const msg = ["toprelay-caller-v1", method, target, f.client, f.ts, f.nonce, bodyHash].join("\n");
  const want = Buffer.from(createHmac("sha256", key).update(msg).digest("hex"));
  const got = Buffer.from(f.sig);
  if (got.length !== want.length || !timingSafeEqual(got, want)) return null;
  try { return decodeURIComponent(f.client); } catch { return null; }
}
```

With `node:http`, the target is `req.url`; collect the body as a `Buffer` first.

## 3. Don't let another program answer for your app

Whatever binds your localhost port receives the relayed requests and the credential. If your app isn't running,
any program can take the port.

**Serve MCP on a Unix socket** (macOS and Linux). Create the socket in a directory only you can write to, and
`chmod 600` it right after `bind`:

```sh
toprelay add myapp --mcp-url "unix://$HOME/Library/Application Support/MyApp/mcp.sock" --header-file … --json
# HTTP path defaults to /mcp; another one: unix:///…/mcp.sock?path=/rpc
```

Before every connection the agent checks that the socket is yours with mode 0600, that its directory is yours (or
root's) and not writable by others, and that the process on the other end runs as you. Requests over a socket
carry `Host: localhost`.

**Or pin the TCP port to your code signature** (macOS):

```sh
toprelay add myapp --mcp-url http://127.0.0.1:7879/mcp --pin-team-id ABCDE12345 --pin-bundle-id com.example.myapp …
```

For each new connection the agent finds the process that accepted it and checks it with
`codesign --verify -R='anchor apple generic and certificate leaf[subject.OU] = "ABCDE12345" and identifier "com.example.myapp"'`.
Find your team id with `codesign -dv --verbose=2 /Applications/MyApp.app` (`TeamIdentifier`). A bundle id alone
is refused: anyone can sign a binary with any bundle id. Limits are listed in
[agent/README.md](agent/README.md#what-the-app-receives); the main ones: an `lsof` and a `codesign` run per new
connection (cached 5 minutes), and if it can't see who accepted the connection (another user's process) it
refuses.
Ad-hoc-signed development builds have no team, so they fail a pin; use a socket while developing.

## 4. The Host header you will see

Relayed requests arrive with **your own** `Host` (exactly the host and port in the URL you gave `add`, e.g.
`127.0.0.1:7879`; `localhost` over a socket), never the relay's. So a DNS-rebinding check that requires
`Host: 127.0.0.1[:port]` or `localhost[:port]` keeps working. The relay's public host is in `X-Forwarded-Host`.
If you registered `http://localhost:7879/mcp`, you get `Host: localhost:7879`.

## 5. Long tool calls: start streaming early

The relay gives up on a reply whose headers take more than 120 s, and ends a stream that sends nothing for 300 s.
For tools that take 30-90 s or more (rendering, OCR):

- **Best: answer with SSE at once.** Send the response headers (`Content-Type: text/event-stream`) as soon as the
  call starts, send `notifications/progress` as you go (if the client sent a `progressToken`), then the result as
  the last event. Progress reaches the bot as it happens.
- **If you can only answer JSON**, it still works: after 20 s without your headers the agent starts an SSE reply
  for you, keeps it alive, and delivers your JSON as the final event. Only single `tools/call` requests get this.
- **Stop work when the bot leaves.** The agent cancels its request to you when the bot disconnects: your HTTP
  request's context or connection ends. Check it in long loops. (The Go SDK's stateless Streamable HTTP handler,
  v1.8, doesn't pass that on to the tool's context; check `r.Context()` in a wrapper if you use it.)
- Nothing may take more than 840 s in total.

## 6. Drive TopRelay without a command line

If your app shells out to `toprelay`, 0.2 makes that safer: every command finishes within `--timeout` (default
5 minutes), `up --install` no longer hangs on `launchctl`, and in `--json` mode stderr is capped at 4 KiB. Keep
reading stdout and stderr to EOF, or send them to files as before.

Better, on macOS: use [TopRelayKit](TopRelayKit/README.md). It talks to the running agent over its control
socket, with the same results as `--json`, a pushed status stream instead of polling, and credentials passed in
the request body instead of arguments.

```swift
let relay = TopRelay()
if try await relay.services().contains(where: { $0.name == "myapp" }) {
    try await relay.setCredential(service: "myapp", .file(header: "X-App-Token", path: tokenPath))
} else {
    _ = try await relay.register(service: "myapp", mcpURL: localURL, credential: .file(header: "X-App-Token", path: tokenPath))
}
```

If you poll `toprelay status --json`, use `toprelay status --watch --json` (one line per change) or watch
`state.json` instead; its schema is in [agent/README.md](agent/README.md#files) and now has a `version` field.

## 7. Return files as links, not base64

Base64 in a JSON tool result costs a third more bytes, is encoded twice, and runs into the relay's limits for
slow readers. Return an MCP `resource_link` instead and let TopRelay serve the file.

**From an outbox** (works for sandboxed apps; no API call):

```sh
toprelay files-dir myapp "$HOME/Library/Containers/com.example.myapp/Data/Outbox" --json
```

Write the file there, then return:

```json
{"type": "resource_link", "uri": "toprelay-file:///report-2026-11.pdf", "name": "Report.pdf", "mimeType": "application/pdf"}
```

**Or register any file of yours** through TopRelayKit, and return the `uri` it gives you:

```swift
let offer = try await relay.offerFile(service: "myapp", path: pdfURL.path, contentType: "application/pdf")
// {"type":"resource_link","uri": offer.uri, …}
```

The bot receives `https://toprelay.<you>.workers.dev/myapp/files/<id>?e=…&s=…`. It needs no credential, works
once, expires after 10 minutes, and is refused if the file changed in the meantime, so leave the file in place
until it has been fetched. If a link can't be made, that item becomes a text item explaining why. A relay deployed
before 0.2 needs `toprelay deploy` once.

Uploads (a bot sending you a large file) aren't supported yet; keep accepting them as tool arguments for now.
