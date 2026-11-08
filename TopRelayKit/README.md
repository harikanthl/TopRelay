# TopRelayKit

A small Swift package (no dependencies, macOS 13+) that lets a Mac app drive TopRelay without starting processes.
It talks to a running `toprelay up` over its control socket, `<config dir>/agent.sock`, and gets back the same
objects as `toprelay … --json` (see [agent/README.md](../agent/README.md#control-socket)). The agent does the work;
the Kit is a thin client, not a second implementation.

```swift
import TopRelayKit

let relay = TopRelay()                       // ~/.config/toprelay, or TOPRELAY_CONFIG_DIR
guard relay.isAgentRunning else { … }        // see "Starting the agent"

let reg = try await relay.register(service: "notes", mcpURL: "unix://\(socketPath)",
                                   credential: .file(header: "X-Notes-Token", path: tokenFile.path))
print(reg.url)                                // give this to the bot

try await relay.setCredential(service: "notes", .file(header: "X-Notes-Token", path: tokenFile.path)) // live
try await relay.tools(service: "notes", allow: ["search", "read"])
let code = try await relay.pairingCode(service: "notes")                  // claude.ai consent page
let key  = try await relay.mintKey(service: "notes", client: "xai", validFor: "30d")
let ck   = try await relay.callerKey(service: "notes")                    // verify X-TopRelay-Caller with ck.key

for try await state in relay.stateUpdates() {                             // pushed, not polled
    let s = state.services["notes"]
    print(state.running, s?.connected ?? false, s?.lastError ?? "", s?.requests ?? 0)
}
```

| Call | CLI equivalent |
|---|---|
| `register(service:mcpURL:credential:toolsDefault:pinTeamID:pinBundleID:)` | `add --mcp-url … --header-file/--header-stdin …` |
| `setCredential(service:_:)` | `set-header --file / --stdin / --clear` |
| `tools(service:allow:deny:allowAll:)` | `tools` |
| `pairingCode(service:validFor:)` | `pair` |
| `mintKey(service:client:tools:validFor:)` | `key` |
| `callerKey(service:)` | `caller-key` |
| `services()`, `remove(service:)` | `list`, `remove` |
| `state()`, `stateUpdates()` | `status --watch --json` |
| `run(_:stdin:)` | any allowed command, raw JSON |

Errors are `TopRelayError` with the CLI's `code`, `message` and `fix`. `agent_not_running` means nothing answers
on the socket.

Credentials go in the request body (the agent reads them as stdin), never in arguments.

## Starting the agent

The Kit needs a running agent. Either the user installs it once (`toprelay up --install`, a LaunchAgent that runs
at login), or the app bundles the `toprelay` binary and registers its own LaunchAgent with `SMAppService.agent`
(a plist in `Contents/Library/LaunchAgents` whose `ProgramArguments` are the bundled binary and `up`). Neither
starts a child process of the app.

## Example

`Sources/TopRelayKitExample/ReachFromAnywhere.swift` is a SwiftUI "Reach from anywhere" toggle: on registers the
service (or, if it exists, refreshes only its credential), shows the URL, connection and call count from
`stateUpdates()`, and makes pairing codes; off removes the service.

## Tests

```sh
swift test                                                   # the client against a fake agent socket
cd ../agent && TOPRELAY_SWIFT_TESTS=1 go test -run TestTopRelayKitEndToEnd ./internal/cli   # against a real `up`
```
