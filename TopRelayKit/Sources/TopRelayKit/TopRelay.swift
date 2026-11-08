import Foundation

/// Drives the TopRelay agent (`toprelay up`) through its control socket, `<config dir>/agent.sock`. No processes
/// are started: every call runs the same code as the CLI inside the agent and returns what `toprelay … --json`
/// would print. The agent must be running (install it once with `toprelay up --install`, or register a bundled
/// copy as a LaunchAgent with `SMAppService`).
public struct TopRelay: Sendable {
    public let configDirectory: URL

    public init(configDirectory: URL = TopRelay.defaultConfigDirectory) {
        self.configDirectory = configDirectory
    }

    /// `TOPRELAY_CONFIG_DIR`, else `$XDG_CONFIG_HOME/toprelay`, else `~/.config/toprelay` (as the CLI).
    public static var defaultConfigDirectory: URL {
        let env = ProcessInfo.processInfo.environment
        if let d = env["TOPRELAY_CONFIG_DIR"], !d.isEmpty { return URL(fileURLWithPath: d) }
        if let x = env["XDG_CONFIG_HOME"], !x.isEmpty { return URL(fileURLWithPath: x).appendingPathComponent("toprelay") }
        let home = env["HOME"].map { URL(fileURLWithPath: $0) } ?? FileManager.default.homeDirectoryForCurrentUser
        return home.appendingPathComponent(".config/toprelay")
    }

    public var socketPath: String { configDirectory.appendingPathComponent("agent.sock").path }

    /// Whether an agent answers on the socket.
    public var isAgentRunning: Bool {
        guard let fd = try? SocketHTTP.connect(socketPath) else { return false }
        close(fd)
        return true
    }

    // MARK: Commands

    /// Runs any allowed command (`add`, `set-header`, `tools`, `list`, `status`, `pair`, `key`, `caller-key`,
    /// `rotate`, `remove`, `doctor`, `test`, `version`, `login`) and returns its JSON object.
    public func run(_ args: [String], stdin: String = "") async throws -> Data {
        let body = try JSONSerialization.data(withJSONObject: ["args": args, "stdin": stdin])
        let socket = socketPath
        let resp = try await Task.detached {
            try SocketHTTP.roundTrip(socket: socket, method: "POST", path: "/v1/cli", body: body)
        }.value
        if resp.status != 200 {
            if let e = try? JSONDecoder().decode(ErrorEnvelope.self, from: resp.body) {
                throw TopRelayError(code: e.error.code, message: e.error.message, fix: e.error.fix)
            }
            throw TopRelayError.io("agent answered HTTP \(resp.status)")
        }
        return resp.body
    }

    func decode<T: Decodable>(_ type: T.Type, _ args: [String], stdin: String = "") async throws -> T {
        let data = try await run(args, stdin: stdin)
        do {
            return try Self.decoder.decode(T.self, from: data)
        } catch {
            throw TopRelayError.io("unexpected answer to \(args.first ?? "?"): \(error)")
        }
    }

    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            let f = ISO8601DateFormatter()
            f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = f.date(from: s) { return date }
            f.formatOptions = [.withInternetDateTime]
            if let date = f.date(from: s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: dec.codingPath, debugDescription: "bad date \(s)"))
        }
        return d
    }()

    /// Registers a local MCP server (`add --mcp-url`). The first registration deploys the relay, which can take a
    /// minute or two.
    public func register(service: String, mcpURL: String, credential: Credential? = nil,
                         toolsDefault: ToolsDefault = .allowReadOnly, pinTeamID: String? = nil,
                         pinBundleID: String? = nil) async throws -> Registration {
        var args = ["add", service, "--mcp-url", mcpURL, "--tools-default", toolsDefault.rawValue]
        var stdin = ""
        switch credential {
        case .file(let header, let path): args += ["--header-file", "\(header)=\(path)"]
        case .value(let header, let value):
            args.append("--header-stdin")
            stdin = "\(header): \(value)\n"
        case nil: break
        }
        if let pinTeamID { args += ["--pin-team-id", pinTeamID] }
        if let pinBundleID { args += ["--pin-bundle-id", pinBundleID] }
        return try await decode(Registration.self, args, stdin: stdin)
    }

    /// Changes the credential header live: no redeploy, keys and OAuth grants keep working. `nil` stops sending one.
    @discardableResult
    public func setCredential(service: String, _ credential: Credential?) async throws -> HeaderUpdate {
        switch credential {
        case .file(let header, let path):
            return try await decode(HeaderUpdate.self, ["set-header", service, "--file", "\(header)=\(path)"])
        case .value(let header, let value):
            return try await decode(HeaderUpdate.self, ["set-header", service, "--stdin"], stdin: "\(header): \(value)\n")
        case nil:
            return try await decode(HeaderUpdate.self, ["set-header", service, "--clear"])
        }
    }

    /// The service's tools and policy, optionally changing it.
    @discardableResult
    public func tools(service: String, allow: [String] = [], deny: [String] = [], allowAll: Bool = false) async throws -> ToolsResult {
        var args = ["tools", service]
        if !allow.isEmpty { args += ["--allow", allow.joined(separator: ",")] }
        if !deny.isEmpty { args += ["--deny", deny.joined(separator: ",")] }
        if allowAll { args.append("--allow-all") }
        return try await decode(ToolsResult.self, args)
    }

    /// A one-time code for the OAuth consent page (claude.ai, ChatGPT).
    public func pairingCode(service: String, validFor minutes: Int = 10) async throws -> PairingCode {
        try await decode(PairingCode.self, ["pair", service, "--ttl", "\(minutes)m"])
    }

    /// A bearer key, with snippets for `client` ("claude-api", "openai", "xai", "gemini", "qwen", "curl").
    public func mintKey(service: String, client: String? = nil, tools: [String] = [], validFor: String? = nil) async throws -> MintedKey {
        var args = ["key", service]
        if let client { args += ["--client", client] }
        if !tools.isEmpty { args += ["--tools", tools.joined(separator: ",")] }
        if let validFor { args += ["--for", validFor] }
        return try await decode(MintedKey.self, args)
    }

    /// The key to verify `X-TopRelay-Caller` with.
    public func callerKey(service: String) async throws -> CallerKey {
        try await decode(CallerKey.self, ["caller-key", service])
    }

    /// Offers a file to bots: put the returned `uri` (`toprelay-file://<id>`) in a tool result's `resource_link`,
    /// and the agent swaps it for a signed, single-use download link. The file must be yours, at most 256 MiB, and
    /// unchanged until it is downloaded. The offer lasts `validFor` seconds (default 1 hour).
    public func offerFile(service: String, path: String, name: String? = nil, contentType: String? = nil,
                          validFor seconds: Int? = nil) async throws -> FileOffer {
        var req: [String: Any] = ["service": service, "path": path]
        if let name { req["name"] = name }
        if let contentType { req["contentType"] = contentType }
        if let seconds { req["ttl"] = seconds }
        let body = try JSONSerialization.data(withJSONObject: req)
        let socket = socketPath
        let resp = try await Task.detached {
            try SocketHTTP.roundTrip(socket: socket, method: "POST", path: "/v1/files", body: body)
        }.value
        if resp.status != 200 {
            if let e = try? JSONDecoder().decode(ErrorEnvelope.self, from: resp.body) {
                throw TopRelayError(code: e.error.code, message: e.error.message, fix: e.error.fix)
            }
            throw TopRelayError.io("agent answered HTTP \(resp.status)")
        }
        return try Self.decoder.decode(FileOffer.self, from: resp.body)
    }

    public func services() async throws -> [ServiceInfo] {
        try await decode(ListResult.self, ["list"]).services
    }

    public func remove(service: String) async throws {
        _ = try await run(["remove", service])
    }

    // MARK: Status

    /// The agent's current state.
    public func state() async throws -> AgentState {
        let socket = socketPath
        let resp = try await Task.detached {
            try SocketHTTP.roundTrip(socket: socket, method: "GET", path: "/v1/state", body: nil)
        }.value
        guard resp.status == 200 else { throw TopRelayError.io("agent answered HTTP \(resp.status)") }
        return try Self.decoder.decode(AgentState.self, from: resp.body)
    }

    /// The agent's state now, then on every change (connected, last error, request counts). Pushed by the agent;
    /// nothing is polled. The stream ends when the agent stops; cancel the task to stop listening.
    public func stateUpdates() -> AsyncThrowingStream<AgentState, Error> {
        let socket = socketPath
        return AsyncThrowingStream { continuation in
            let box = FDBox()
            let thread = Thread {
                do {
                    try SocketHTTP.stream(socket: socket, path: "/v1/state/watch", fdReady: { box.set($0) }, fdDone: { _ in box.clear() }) { line in
                        if let s = try? Self.decoder.decode(AgentState.self, from: line) { continuation.yield(s) }
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in box.shutdown() }
            thread.start()
        }
    }
}

/// Holds the stream's socket so cancellation can unblock the reading thread.
final class FDBox: @unchecked Sendable {
    private let lock = NSLock()
    private var fd: Int32 = -1
    private var cancelled = false

    func set(_ fd: Int32) {
        lock.lock()
        defer { lock.unlock() }
        self.fd = fd
        if cancelled { Darwin.shutdown(fd, SHUT_RDWR) }
    }

    /// The socket is about to be closed: forget its number.
    func clear() {
        lock.lock()
        defer { lock.unlock() }
        fd = -1
    }

    func shutdown() {
        lock.lock()
        defer { lock.unlock() }
        cancelled = true
        if fd >= 0 { Darwin.shutdown(fd, SHUT_RDWR) }
    }
}
