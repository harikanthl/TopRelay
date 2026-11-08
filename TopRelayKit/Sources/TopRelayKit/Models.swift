import Foundation

/// A failure, with the same `code`, `message` and `fix` the CLI prints in `--json` mode.
public struct TopRelayError: Error, LocalizedError, Equatable, Sendable {
    public let code: String
    public let message: String
    public let fix: String?

    public init(code: String, message: String, fix: String? = nil) {
        self.code = code
        self.message = message
        self.fix = fix
    }

    public var errorDescription: String? { fix.map { "\(message). \($0)" } ?? message }

    static func agentNotRunning(_ path: String) -> TopRelayError {
        TopRelayError(code: "agent_not_running", message: "TopRelay isn't running (no agent at \(path))",
                      fix: "start it once with `toprelay up --install`; it then runs at login")
    }

    static func io(_ message: String) -> TopRelayError { TopRelayError(code: "io", message: message) }
}

struct ErrorEnvelope: Decodable {
    struct Body: Decodable {
        let code: String
        let message: String
        let fix: String?
    }
    let error: Body
}

/// How new or unlisted tools are exposed when a service is registered.
public enum ToolsDefault: String, Sendable {
    case allowReadOnly = "allow-readonly"
    case deny
    case allowAll = "allow-all"
}

/// Where the agent gets the app's credential header.
public enum Credential: Sendable, Equatable {
    /// Read from this file (owned by you, mode 0600) on every request. Rotate by rewriting the file.
    case file(header: String, path: String)
    /// Kept in the OS keyring. Sent over the socket, never on a command line.
    case value(header: String, value: String)
}

/// One tool as `add` and `tools` show it.
public struct ToolView: Decodable, Sendable, Equatable {
    public let name: String
    public let title: String?
    public let description: String?
    public let allowed: Bool
    public let readOnly: Bool
    public let destructive: Bool
}

/// The result of `register` (`toprelay add --json`).
public struct Registration: Decodable, Sendable {
    public let url: String
    public let sseUrl: String?
    public let tools: [ToolView]
    public let warnings: [String]
    public let firstDeploy: Bool
    public let subdomain: String
}

/// `toprelay tools --json`.
public struct ToolsResult: Decodable, Sendable {
    public struct Policy: Decodable, Sendable {
        public let allow: [String]
        public let deny: [String]
        public let `default`: String
    }
    public let service: String
    public let policy: Policy
    public let reachable: Bool
    public let error: String?
    public let tools: [ToolView]
    public let changed: Bool
}

/// `toprelay set-header --json`.
public struct HeaderUpdate: Decodable, Sendable {
    public let service: String
    public let header: String
    public let updated: Bool
    public let source: String
}

/// `toprelay pair --json`.
public struct PairingCode: Decodable, Sendable {
    public let service: String
    public let code: String
    public let expires: Int64
    public var expiresAt: Date { Date(timeIntervalSince1970: TimeInterval(expires)) }
}

/// `toprelay key --json`: a bearer key and ready-made snippets per client.
public struct MintedKey: Decodable, Sendable {
    public let key: String
    public let id: String
    public let service: String
    public let tools: [String]?
    public let expires: Int64?
    public let url: String
    public let snippets: [String: String]
}

/// `toprelay caller-key --json`: what the app needs to verify `X-TopRelay-Caller`.
public struct CallerKey: Decodable, Sendable {
    public let service: String
    public let header: String
    public let key: String
    public let path: String
    public let source: String
}

/// One service in `list`.
public struct ServiceInfo: Decodable, Sendable {
    public let name: String
    public let kind: String
    public let local: String
    public let url: String
    public let allowedTools: [String]
    public let deniedTools: [String]
    public let upstreamHeader: String?
    public let upstreamHeaderSource: String?
}

struct ListResult: Decodable {
    let services: [ServiceInfo]
}

/// The live state of the local agent (state.json, schema `version` 1).
public struct AgentState: Decodable, Sendable, Equatable {
    public struct Service: Decodable, Sendable, Equatable {
        public let connected: Bool
        public let since: Date?
        public let lastError: String
        public let reconnectAttempt: Int
        public let nextRetryMs: Int64
        public let requests: Int64
        public let refused: Int64
    }
    public let version: Int
    public let pid: Int
    public let running: Bool
    public let updatedAt: Date
    public let services: [String: Service]
}

/// A file offered to bots (`offerFile`): return `uri` in a `resource_link`.
public struct FileOffer: Decodable, Sendable {
    public let service: String
    public let id: String
    public let uri: String
    public let expires: Int64
}
