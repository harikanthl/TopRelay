// The app's state: what the engine reports, plus the actions the UI can take. Live connection status comes
// from state.json (written by `toprelay up`); relay-side numbers come from `toprelay status`.

import AppKit
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
        keepsRunning = EnginePaths.installedAgentExecutable() != nil
        guard let data = try? Data(contentsOf: EnginePaths.stateFile),
              let state = try? JSONDecoder().decode(DaemonState.self, from: data) else {
            if daemon != nil { daemon = nil }
            return
        }
        // A state file left by a process that is gone says "running" forever; trust the pid.
        let alive = state.running && kill(pid_t(state.pid), 0) == 0
        let effective = alive ? state : DaemonState(pid: state.pid, startedAt: state.startedAt, updatedAt: state.updatedAt, running: false, services: state.services)
        if effective != daemon { daemon = effective }
    }

    /// Reloads the service list (local config only, no network).
    func reload() async {
        guard let engine else { return }
        do {
            let result: ListResult = try await engine.run(["list"])
            list = result
            phase = result.accountId.isEmpty ? .needsLogin : .ready
            lastError = nil
        } catch let failure as EngineFailure where ["no_config", "not_logged_in"].contains(failure.code) {
            list = nil
            phase = .needsLogin
        } catch let failure as EngineFailure {
            lastError = failure
            if phase == .loading { phase = .needsLogin }
        } catch {
            lastError = EngineFailure(code: "error", message: error.localizedDescription)
        }
    }

    /// Asks the relay for per-service numbers. Needs the network; failures are kept, not shown as alerts.
    func refreshStatus() async {
        guard let engine, phase == .ready, !services.isEmpty, !isRefreshingStatus else { return }
        isRefreshingStatus = true
        defer { isRefreshingStatus = false }
        if let result: StatusResult = try? await engine.run(["status"]) {
            status = result
        }
    }

    // MARK: Keeping the engine running (login item)

    func setKeepsRunning(_ on: Bool) async throws {
        guard let engine else { return }
        let _: InstallResult = try await engine.run(["up", on ? "--install" : "--uninstall"])
        readState()
    }

    /// The login item points at the engine inside this app. If the app was moved (or updated in place to a new
    /// path), reinstall so it points at this copy.
    private func repairLoginItemIfMoved() async {
        guard let engine, let installed = EnginePaths.installedAgentExecutable() else { return }
        let current = engine.executable.resolvingSymlinksInPath().path
        if installed != current {
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
        let result: AddResult = try await engine.run(args)
        await reload()
        await refreshStatus()
        return result
    }

    func remove(_ service: String) async throws {
        guard let engine else { return }
        let _: RemoveResult = try await engine.run(["remove", service])
        await reload()
        await refreshStatus()
    }

    func rotate(_ service: String) async throws -> RotateResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        let result: RotateResult = try await engine.run(["rotate", service])
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
        return try await engine.run(["test", service])
    }

    func doctor() async throws -> DoctorResult {
        guard let engine else { throw EngineFailure(code: "engine_missing", message: "TopRelay's engine is missing.") }
        return try await engine.run(["doctor"])
    }

    // MARK: Helpers

    func copy(_ text: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
    }

    /// Reads the most recent calls from the local call log (newest first).
    nonisolated static func recentCalls(service: String, limit: Int = 200) -> [CallRecord] {
        guard let data = try? Data(contentsOf: EnginePaths.logFile(for: service)) else { return [] }
        let decoder = JSONDecoder()
        let lines = data.split(separator: UInt8(ascii: "\n")).suffix(limit)
        return lines.compactMap { try? decoder.decode(CallRecord.self, from: Data($0)) }.reversed()
    }
}
