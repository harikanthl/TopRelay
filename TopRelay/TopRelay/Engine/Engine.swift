// Runs the bundled `toprelay` engine (Contents/Helpers/toprelay) with --json and decodes its one JSON object.
// The app never talks to the relay itself: every network action goes through the engine.

import Foundation

nonisolated struct Engine: Sendable {
    let executable: URL
    let environment: [String: String]

    /// The engine bundled by the "Bundle toprelay engine" build phase.
    static func bundled() -> Engine? {
        let url = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/toprelay")
        guard FileManager.default.isExecutableFile(atPath: url.path) else { return nil }
        return Engine(executable: url, environment: EnginePaths.childEnvironment())
    }

    /// Runs `toprelay --json <args>` and decodes the result, or throws the engine's own error. The engine is
    /// stopped if it runs longer than `timeout` seconds or the calling task is cancelled.
    func run<T: Decodable & Sendable>(_ args: [String], stdin: String? = nil, timeout: TimeInterval = 60, as type: T.Type = T.self) async throws -> T {
        let outcome = try await execute(["--json"] + args, stdin: stdin, timeout: timeout)
        if outcome.timedOut {
            throw EngineFailure(code: "timeout", message: "The engine took longer than \(Int(timeout)) seconds and was stopped.",
                                fix: "Check your network, then try again. `toprelay doctor` in Terminal shows more.")
        }
        let decoder = JSONDecoder()
        if let failure = try? decoder.decode(FailureEnvelope.self, from: outcome.data) { throw failure.error }
        do {
            return try decoder.decode(T.self, from: outcome.data)
        } catch {
            let text = String(decoding: outcome.data.prefix(400), as: UTF8.self)
            throw EngineFailure(
                code: "bad_output",
                message: outcome.status == 0 ? "The engine's answer couldn't be read." : "The engine stopped with an error (exit \(outcome.status)).",
                fix: text.isEmpty ? "Run `toprelay doctor` in Terminal for details." : text
            )
        }
    }

    private struct Outcome: Sendable {
        let data: Data
        let status: Int32
        let timedOut: Bool
    }

    /// Holds the running process so a timeout or a cancelled task can stop it from another thread.
    private final class Running: @unchecked Sendable {
        private let lock = NSLock()
        private var process: Process?
        private var timedOut = false
        private var stopped = false

        func set(_ process: Process) -> Bool {
            lock.withLock {
                self.process = process
                return !stopped
            }
        }

        func stop(timedOut: Bool) {
            lock.withLock {
                stopped = true
                if timedOut { self.timedOut = true }
                if let process, process.isRunning { process.terminate() }
            }
        }

        var didTimeOut: Bool { lock.withLock { timedOut } }
    }

    /// Runs the engine and returns stdout and the exit status. Stdout is read on a background queue while the
    /// process runs, so a large answer can't fill the pipe and stall it. Stderr carries only human notes in
    /// --json mode and is discarded. The engine's own children get their own pipes, so once it exits (or is
    /// terminated) stdout reaches end of file.
    private func execute(_ args: [String], stdin: String?, timeout: TimeInterval) async throws -> Outcome {
        let executable = self.executable
        let environment = self.environment
        let running = Running()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Outcome, Error>) in
                DispatchQueue.global(qos: .userInitiated).async {
                    let process = Process()
                    process.executableURL = executable
                    process.arguments = args
                    process.environment = environment
                    let out = Pipe()
                    process.standardOutput = out
                    process.standardError = FileHandle.nullDevice
                    let input = Pipe()
                    process.standardInput = stdin == nil ? FileHandle.nullDevice : input
                    guard running.set(process) else {
                        continuation.resume(throwing: CancellationError())
                        return
                    }
                    do {
                        try process.run()
                    } catch {
                        continuation.resume(throwing: EngineFailure(
                            code: "engine_missing",
                            message: "TopRelay's engine couldn't start.",
                            fix: "Reinstall TopRelay. (\(error.localizedDescription))"
                        ))
                        return
                    }
                    DispatchQueue.global().asyncAfter(deadline: .now() + timeout) { running.stop(timedOut: true) }
                    if let stdin {
                        // SIGPIPE is ignored app-wide (TopRelayApp.init), so an engine that exits early makes this
                        // throw instead of killing the app.
                        try? input.fileHandleForWriting.write(contentsOf: Data(stdin.utf8))
                        try? input.fileHandleForWriting.close()
                    }
                    let data = out.fileHandleForReading.readDataToEndOfFile()
                    process.waitUntilExit()
                    continuation.resume(returning: Outcome(data: data, status: process.terminationStatus, timedOut: running.didTimeOut))
                }
            }
        } onCancel: {
            running.stop(timedOut: false)
        }
    }
}

/// Where the engine keeps its files. Mirrors agent/internal/config: $TOPRELAY_CONFIG_DIR, then
/// $XDG_CONFIG_HOME/toprelay, then ~/.config/toprelay.
nonisolated enum EnginePaths {
    static var configDirectory: URL {
        let env = ProcessInfo.processInfo.environment
        if let dir = env["TOPRELAY_CONFIG_DIR"], !dir.isEmpty { return URL(fileURLWithPath: dir, isDirectory: true) }
        if let xdg = env["XDG_CONFIG_HOME"], !xdg.isEmpty {
            return URL(fileURLWithPath: xdg, isDirectory: true).appendingPathComponent("toprelay", isDirectory: true)
        }
        return FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".config", isDirectory: true)
            .appendingPathComponent("toprelay", isDirectory: true)
    }

    static var stateFile: URL { configDirectory.appendingPathComponent("state.json") }
    static var configFile: URL { configDirectory.appendingPathComponent("config.json") }
    static func logFile(for service: String) -> URL {
        configDirectory.appendingPathComponent("logs", isDirectory: true).appendingPathComponent("\(service).jsonl")
    }

    /// The login item written by `toprelay up --install`.
    static var launchAgent: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/LaunchAgents/dev.toprelay.agent.plist")
    }

    /// The canonical path of a file: symlinks resolved and /tmp → /private/tmp, the same answer Go's
    /// filepath.EvalSymlinks gives. (URL.resolvingSymlinksInPath strips /private, which Go keeps.)
    static func canonical(_ path: String) -> String {
        guard let resolved = realpath(path, nil) else { return path }
        defer { free(resolved) }
        return String(cString: resolved)
    }

    /// macOS runs apps opened straight from Downloads (still quarantined) from a temporary, randomized copy.
    /// A login item pointing there breaks after the app quits.
    static var isTranslocated: Bool { Bundle.main.bundlePath.contains("/AppTranslocation/") }

    /// The program path the installed login item runs, if it is installed.
    static func installedAgentExecutable() -> String? {
        guard let data = try? Data(contentsOf: launchAgent),
              let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
              let args = plist["ProgramArguments"] as? [String] else { return nil }
        return args.first
    }

    /// Apps opened from Finder get a minimal PATH. Stdio servers are often started as `node`, `npx` or `uvx`,
    /// so give the engine the same install locations a login shell would have.
    static func childEnvironment() -> [String: String] {
        var env = ProcessInfo.processInfo.environment
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let extra = [".local/bin", ".bun/bin", ".cargo/bin", ".deno/bin", "go/bin", ".volta/bin"].map { "\(home)/\($0)" }
            + ["/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"]
        var seen = Set<String>()
        let dirs = ((env["PATH"] ?? "").split(separator: ":").map(String.init) + extra).filter { !$0.isEmpty && seen.insert($0).inserted }
        env["PATH"] = dirs.joined(separator: ":")
        return env
    }
}
