// Decodable mirrors of the toprelay engine's `--json` output (agent/README.md "JSON output").
// Go encodes nil slices as null, so every array is optional here.

import Foundation

nonisolated struct EngineFailure: Error, Decodable, Sendable, Equatable, LocalizedError {
    let code: String
    let message: String
    let fix: String?
    let accounts: [AccountChoice]?

    init(code: String, message: String, fix: String? = nil, accounts: [AccountChoice]? = nil) {
        self.code = code
        self.message = message
        self.fix = fix
        self.accounts = accounts
    }

    var errorDescription: String? { message }
    var recoverySuggestion: String? { fix }
}

nonisolated struct FailureEnvelope: Decodable, Sendable {
    let error: EngineFailure
}

nonisolated struct AccountChoice: Decodable, Sendable, Hashable, Identifiable {
    let id: String
    let name: String
}

nonisolated struct LoginResult: Decodable, Sendable {
    let account: AccountChoice
    let tokenStatus: String
}

nonisolated struct ListResult: Decodable, Sendable, Equatable {
    let accountId: String
    let publicUrl: String
    let services: [ListedService]?
}

nonisolated struct ListedService: Decodable, Sendable, Identifiable, Hashable {
    var id: String { name }
    let name: String
    let kind: String
    /// Human-readable source, e.g. "mcp-url http://127.0.0.1:7879/mcp" or "stdio node server.js".
    let source: String
    let local: String
    let url: String
    let sseUrl: String?
    let gen: Int
    let allowedTools: [String]?
    let deniedTools: [String]?
    let newTools: String
    let allowPaths: [String]?
    let upstreamHeader: String?

    var isMCP: Bool { kind == "mcp" }
}

/// One service's entry in state.json, written by `toprelay up`.
nonisolated struct LocalServiceState: Decodable, Sendable, Equatable {
    let connected: Bool
    let since: String?
    let lastError: String
    let reconnectAttempt: Int
    let nextRetryMs: Int64
    let requests: Int64
    let refused: Int64
}

/// state.json, written atomically by `toprelay up` on every change.
nonisolated struct DaemonState: Decodable, Sendable, Equatable {
    let pid: Int
    let startedAt: String
    let updatedAt: String
    let running: Bool
    let services: [String: LocalServiceState]?
}

nonisolated struct StatusResult: Decodable, Sendable, Equatable {
    let day: String
    let requestsToday: Int64
    let freeLimit: Int64
    let percentOfLimit: Double
    let daemon: DaemonState?
    let services: [RemoteServiceStatus]?
}

nonisolated struct RemoteServiceStatus: Decodable, Sendable, Equatable, Identifiable {
    var id: String { name }
    let name: String
    let kind: String
    let url: String
    let connected: Bool
    let connectedAt: Int64?
    let agent: String?
    let requests: Int64
    let rejected: Int64
    let bytesIn: Int64
    let bytesOut: Int64
    let revokedKeys: Int
    let error: String?
}

nonisolated struct ToolPolicy: Decodable, Sendable, Equatable {
    let allow: [String]?
    let deny: [String]?
    let `default`: String
}

nonisolated struct ToolView: Decodable, Sendable, Identifiable, Hashable {
    var id: String { name }
    let name: String
    let title: String?
    let description: String?
    let allowed: Bool
    let readOnly: Bool
    let destructive: Bool
    let idempotent: Bool
    let openWorld: Bool
    let warnings: [String]?
}

nonisolated struct ToolsResult: Decodable, Sendable, Equatable {
    let service: String
    let policy: ToolPolicy
    let reachable: Bool
    let error: String?
    let tools: [ToolView]?
    let changed: Bool
}

nonisolated struct AddResult: Decodable, Sendable {
    let url: String
    let sseUrl: String?
    let tools: [ToolView]?
    let warnings: [String]?
    let firstDeploy: Bool
    let subdomain: String
}

nonisolated struct PairResult: Decodable, Sendable, Equatable {
    let service: String
    let code: String
    let expires: Int64

    var expiryDate: Date { Date(timeIntervalSince1970: TimeInterval(expires)) }
    /// ABCD-EFGH-JKMN: easier to read aloud and type. The relay ignores dashes and case.
    var grouped: String {
        stride(from: 0, to: code.count, by: 4).map { i -> String in
            let start = code.index(code.startIndex, offsetBy: i)
            let end = code.index(start, offsetBy: min(4, code.count - i))
            return String(code[start..<end])
        }.joined(separator: "-")
    }
}

nonisolated struct KeyResult: Decodable, Sendable, Equatable {
    let key: String
    let id: String
    let service: String?
    let tools: [String]?
    let expires: Int64?
    let url: String?
    let snippets: [String: String]?
    let warnings: [String]?
}

nonisolated struct KeyRevokeResult: Decodable, Sendable {
    let service: String
    let id: String
    let revokedKeys: Int
}

nonisolated struct RotateResult: Decodable, Sendable {
    let service: String
    let gen: Int
    let oauthRevoked: Int
    let warnings: [String]?
}

nonisolated struct RemoveResult: Decodable, Sendable {
    let service: String
    let removed: Bool
    let oauthRevoked: Int
    let warnings: [String]?
}

nonisolated struct DoctorCheck: Decodable, Sendable, Identifiable, Hashable {
    var id: String { name }
    let name: String
    let status: String
    let detail: String?
    let fix: String?
}

nonisolated struct DoctorResult: Decodable, Sendable, Equatable {
    let ok: Bool
    let checks: [DoctorCheck]?
}

nonisolated struct TestResult: Decodable, Sendable, Equatable {
    let service: String
    let call: String
    let ok: Bool
    let status: Int?
    let tools: [String]?
    let remoteMs: Double
    let localMs: Double
    let relayAddedMs: Double
    let `protocol`: String?
}

nonisolated struct InstallResult: Decodable, Sendable {
    let installed: Bool
    let path: String
}

/// One line of <config>/logs/<service>.jsonl.
nonisolated struct CallRecord: Decodable, Sendable, Identifiable, Hashable {
    /// Stable across re-reads of the log, so the table only redraws when calls change. `seq` is unique per line
    /// in chained logs; older lines without it fall back to their fields.
    var id: String { seq.map { "#\($0)" } ?? "\(time)|\(method)|\(path)|\(status)|\(durationMs)" }
    let time: String
    let method: String
    let path: String
    let mcpMethod: String?
    let tool: String?
    let status: Int
    let durationMs: Int64
    let client: String?
    let refused: Bool?
    /// Shield findings: credential rules redacted (or withheld, when `blocked`) and instruction-like text.
    let leaks: [String]?
    let blocked: Bool?
    let warnings: [String]?
    /// Audit chain: lines with a `type` (checkpoint, prune) are bookkeeping, not calls.
    let type: String?
    let seq: UInt64?

    var isCall: Bool { type == nil }
    var hadLeak: Bool { !(leaks ?? []).isEmpty }

    var date: Date? { Timestamps.parse(time) }
    var label: String { tool ?? mcpMethod ?? "\(method) \(path)" }
    var wasRefused: Bool { refused == true }
}

nonisolated enum Timestamps {
    static func parse(_ text: String?) -> Date? {
        guard let text else { return nil }
        let withFraction = ISO8601DateFormatter()
        withFraction.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = withFraction.date(from: text) { return d }
        return ISO8601DateFormatter().date(from: text)
    }
}
