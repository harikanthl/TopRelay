// The app's state: what the engine reports, plus the actions the UI can take. Live connection status comes
// from state.json (written by `toprelay up`); relay-side numbers come from `toprelay status`.

import AppKit
import Darwin
import Foundation
import Observation

/// What the main window shows.
enum Destination: Hashable {
    case service(String)
    case add
    case health
    case settings
}

@MainActor
@Observable
final class AppModel {
    /// The main window's selection; the menu bar sets it before opening the window.
    var destination: Destination? = nil

    enum Phase: Equatable {
        case loading
        /// No engine in the bundle: a broken install.
        case engineMissing
        /// Not signed in to Cloudflare yet.
        case needsLogin
        /// Signed in; may have zero services.
        case ready
        /// The engine answered with an error we can't recover from on our own (shown with a Retry button).
        case failed(EngineFailure)
    }

    enum Health: Equatable {
        case allConnected, someOffline, engineStopped, noServices
    }

    private(set) var phase: Phase = .loading
    private(set) var list: ListResult?
    private(set) var daemon: DaemonState?
    private(set) var status: StatusResult?
    private(set) var lastError: EngineFailure?
    private(set) var isRefreshingStatus = false
    private(set) var keepsRunning = false

    let engine: Engine?
    private var watcher: DirectoryWatcher?
    private var statusTask: Task<Void, Never>?

    init(engine: Engine? = Engine.bundled()) {
        self.engine = engine
    }

    var services: [ListedService] { list?.services ?? [] }

    func localState(for service: String) -> LocalServiceState? {
        guard let daemon, daemon.running else { return nil }
        return daemon.services?[service]
    }

    func remoteStatus(for service: String) -> RemoteServiceStatus? {
        status?.services?.first { $0.name == service }
    }

    var health: Health {
        if services.isEmpty { return .noServices }
        guard let daemon, daemon.running else { return .engineStopped }
        let allUp = services.allSatisfy { daemon.services?[$0.name]?.connected == true }
        return allUp ? .allConnected : .someOffline
    }

    // MARK: Lifecycle

    func start() {
        guard watcher == nil else { return }
        guard engine != nil else {
            phase = .engineMissing
            return
        }
        try? FileManager.default.createDirectory(at: EnginePaths.configDirectory, withIntermediateDirectories: true)
        let watcher = DirectoryWatcher(directory: EnginePaths.configDirectory) { [weak self] in
            self?.readState()
        }
        watcher.start()
        self.watcher = watcher
        readState()
        Task {
            await reload()
            await repairLoginItemIfMoved()
            await refreshStatus()
        }
        statusTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(60))
                await self?.refreshStatus()
            }
        }
    }

    /// Reads state.json. Cheap: called on every directory change.
    func readState() {
        let installed = EnginePaths.installedAgentExecutable() != nil
        if installed != keepsRunning { keepsRunning = installed }
        guard let data = try? Data(contentsOf: EnginePaths.stateFile),
              let state = try? JSONDecoder().decode(DaemonState.self, from: data) else {
            if daemon != nil { daemon = nil }
            return
        }
        // A state file left by a process that is gone says "running" forever; check that the pid is alive and is
        // really the engine (a pid can be reused by another of the user's processes after a crash).
        let alive = state.running && Self.isEngineProcess(pid: state.pid)
        let effective = alive ? state : DaemonState(pid: state.pid, startedAt: state.startedAt, updatedAt: state.updatedAt, running: false, services: state.services)
        if effective != daemon { daemon = effective }
    }

    nonisolated static func isEngineProcess(pid: Int) -> Bool {
        guard pid > 0, kill(pid_t(pid), 0) == 0 else { return false }
        var buffer = [CChar](repeating: 0, count: Int(MAXPATHLEN) * 4)
        let length = proc_pidpath(pid_t(pid), &buffer, UInt32(buffer.count))
        guard length > 0 else { return false }
        return URL(fileURLWithPath: String(cString: buffer)).lastPathComponent == "toprelay"
    }

    /// Reloads the service list (local config only, no network).
    func reload() async {
        guard let engine else { return }
        do {
            let result: ListResult = try await engine.run(["list"])
            list = result
            phase = result.accountId.isEmpty ? .needsLogin : .ready
            lastError = nil
            if (result.services ?? []).isEmpty { status = nil }
        } catch let failure as EngineFailure where ["no_config", "not_logged_in"].contains(failure.code) {
            list = nil
            phase = .needsLogin
        } catch {
            let failure = error.asFailure
            lastError = failure
            // Only a fresh start falls back to the error screen; once running, keep showing the last good list.
            if list == nil { phase = .failed(failure) }
        }
    }

    /// Asks the relay for per-service numbers. Needs the network; failures are kept, not shown as alerts.
    func refreshStatus() async {
        guard let engine, phase == .ready, !services.isEmpty, !isRefreshingStatus else { return }
        isRefreshingStatus = true
        defer { isRefreshingStatus = false }
        if let result: StatusResult = try? await engine.run(["status"], timeout: 45) {
            status = result
        }
    }

    // MARK: Keeping the engine running (login item)

    func setKeepsRunning(_ on: Bool) async throws {
        guard let engine else { return }
        if on && EnginePaths.isTranslocated {
            throw EngineFailure(code: "translocated",
                                message: "TopRelay is running from a temporary copy that macOS removes when it quits.",
                                fix: "Move TopRelay to your Applications folder, open it from there, and try again.")
        }
        let _: InstallResult = try await engine.run(["up", on ? "--install" : "--uninstall"], timeout: 30)
        readState()
    }

    /// The login item points at the engine inside this app. If the app was moved (or updated in place to a new
    /// path), reinstall so it points at this copy.
    private func repairLoginItemIfMoved() async {
        guard let engine, !EnginePaths.isTranslocated, let installed = EnginePaths.installedAgentExecutable() else { return }
        if EnginePaths.canonical(installed) != EnginePaths.canonical(engine.executable.path) {
            try? await setKeepsRunning(true)
        }
    }

    // MARK: Actions (each throws the engine's error for the view to show)

    func login(token: String, account: String? = nil) async throws -> LoginResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        var args = ["login", "--token-stdin"]
        if let account { args += ["--account", account] }
        // No reload here: the setup screen shows its "connected" step first, then reloads.
        return try await engine.run(args, stdin: token + "\n")
    }

    enum NewService {
        case mcpURL(String)
        case stdio(command: [String], cwd: String?)
        case http(String, paths: [String])
    }

    func add(name: String, source: NewService, toolsDefault: String, header: String?) async throws -> AddResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        var args = ["add", name]
        if let header, !header.isEmpty { args += ["--header", header] }
        switch source {
        case .mcpURL(let url):
            args += ["--mcp-url", url, "--tools-default", toolsDefault]
        case .http(let url, let paths):
            args += ["--http", url]
            for path in paths where !path.isEmpty { args += ["--allow-path", path] }
        case .stdio(let command, let cwd):
            args += ["--stdio", "--tools-default", toolsDefault]
            if let cwd, !cwd.isEmpty { args += ["--cwd", cwd] }
            args += ["--"] + command
        }
        let result: AddResult = try await engine.run(args, timeout: 180)
        await reload()
        await refreshStatus()
        return result
    }

    func remove(_ service: String) async throws {
        guard let engine else { return }
        let _: RemoveResult = try await engine.run(["remove", service], timeout: 180)
        await reload()
        await refreshStatus()
    }

    func rotate(_ service: String) async throws -> RotateResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        let result: RotateResult = try await engine.run(["rotate", service], timeout: 180)
        await reload()
        return result
    }

    func pair(_ service: String) async throws -> PairResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        return try await engine.run(["pair", service])
    }

    func mintKey(_ service: String, tools: [String], validFor: String?) async throws -> KeyResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        var args = ["key", service]
        if !tools.isEmpty { args += ["--tools", tools.joined(separator: ",")] }
        if let validFor, !validFor.isEmpty { args += ["--for", validFor] }
        return try await engine.run(args)
    }

    func revokeKey(_ service: String, id: String) async throws -> KeyRevokeResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        return try await engine.run(["key", "revoke", service, id])
    }

    func tools(_ service: String) async throws -> ToolsResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        return try await engine.run(["tools", service])
    }

    func setTool(_ service: String, tool: String, allowed: Bool) async throws -> ToolsResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        let result: ToolsResult = try await engine.run(["tools", service, allowed ? "--allow" : "--deny", tool])
        await reload()
        return result
    }

    func test(_ service: String) async throws -> TestResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        return try await engine.run(["test", service], timeout: 120)
    }

    func doctor() async throws -> DoctorResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        return try await engine.run(["doctor"], timeout: 150)
    }

    // MARK: Helpers

    func copy(_ text: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
    }

    /// Reads the most recent calls from the local call log (newest first). Reads only the end of the file, and
    /// the rotated `.1` file when the current one is short (right after rotation).
    nonisolated static func recentCalls(service: String, limit: Int = 200) -> [CallRecord] {
        let current = EnginePaths.logFile(for: service)
        var lines = tailLines(of: current, limit: limit)
        if lines.count < limit {
            let rotated = current.appendingPathExtension("1")
            lines = tailLines(of: rotated, limit: limit - lines.count) + lines
        }
        let decoder = JSONDecoder()
        return lines.compactMap { try? decoder.decode(CallRecord.self, from: $0) }.filter(\.isCall).reversed()
    }

    /// The last `limit` complete lines of a file, reading at most 256 KiB from its end.
    nonisolated static func tailLines(of url: URL, limit: Int, maxBytes: UInt64 = 256 * 1024) -> [Data] {
        guard limit > 0, let handle = try? FileHandle(forReadingFrom: url) else { return [] }
        defer { try? handle.close() }
        guard let size = try? handle.seekToEnd() else { return [] }
        let start = size > maxBytes ? size - maxBytes : 0
        guard (try? handle.seek(toOffset: start)) != nil, let chunk = try? handle.readToEnd() else { return [] }
        var parts = chunk.split(separator: UInt8(ascii: "\n"), omittingEmptySubsequences: true)
        if start > 0, !parts.isEmpty { parts.removeFirst() } // the first line was cut in half
        return parts.suffix(limit).map { Data($0) }
    }
}
