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

    /// Runs `toprelay --json <args>` and decodes the result, or throws the engine's own error.
    func run<T: Decodable & Sendable>(_ args: [String], stdin: String? = nil, as type: T.Type = T.self) async throws -> T {
        let (data, status) = try await execute(["--json"] + args, stdin: stdin)
        let decoder = JSONDecoder()
        if let failure = try? decoder.decode(FailureEnvelope.self, from: data) { throw failure.error }
        do {
            return try decoder.decode(T.self, from: data)
        } catch {
            let text = String(decoding: data.prefix(400), as: UTF8.self)
            throw EngineFailure(
                code: "bad_output",
                message: status == 0 ? "The engine's answer couldn't be read." : "The engine stopped with an error (exit \(status)).",
                fix: text.isEmpty ? "Run `toprelay doctor` in Terminal for details." : text
            )
        }
    }

    /// Runs the engine and returns stdout and the exit status. Stdout is read on a background queue while the
    /// process runs, so a large answer can't fill the pipe and stall it. Stderr carries only human notes in
    /// --json mode and is discarded.
    private func execute(_ args: [String], stdin: String?) async throws -> (Data, Int32) {
        let executable = self.executable
        let environment = self.environment
        return try await withCheckedThrowingContinuation { continuation in
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
                if let stdin {
                    input.fileHandleForWriting.write(Data(stdin.utf8))
                    try? input.fileHandleForWriting.close()
                }
                let data = out.fileHandleForReading.readDataToEndOfFile()
                process.waitUntilExit()
                continuation.resume(returning: (data, process.terminationStatus))
            }
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
