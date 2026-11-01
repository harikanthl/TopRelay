// Share an app: an MCP server at a URL, an MCP server started as a command, or a local web app.

import SwiftUI

struct AddServiceView: View {
    private enum Kind: String, CaseIterable, Identifiable {
        case mcpURL = "MCP server (URL)"
        case stdio = "MCP server (command)"
        case http = "Web app (HTTP)"
        var id: String { rawValue }
    }

    private enum ToolsDefault: String, CaseIterable, Identifiable {
        case readOnly = "allow-readonly"
        case none = "deny"
        case all = "allow-all"
        var id: String { rawValue }
        var label: String {
            switch self {
            case .readOnly: "Read-only tools (recommended)"
            case .none: "None: I'll pick them"
            case .all: "All tools"
            }
        }
    }

    private static let reserved: Set<String> = ["health", "authorize", "oauth", "well", "admin", "connect", "mcp", "toprelay", "narrow", "revoke"]

    @Environment(AppModel.self) private var model
    @State private var name = ""
    @State private var kind: Kind = .mcpURL
    @State private var url = "http://127.0.0.1:"
    @State private var command = ""
    @State private var cwd = ""
    @State private var paths = ""
    @State private var toolsDefault: ToolsDefault = .readOnly
    @State private var header = ""
    @State private var result: AddResult?
    @State private var addedName = ""
    @State private var action = ActionState()

    var body: some View {
        Form {
            if let result {
                success(result)
            } else {
                formFields
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Share an app")
    }

    // MARK: Form

    @ViewBuilder
    private var formFields: some View {
        Section {
            TextField("Name", text: $name, prompt: Text("notes"))
                .onChange(of: name) { _, new in
                    let cleaned = String(new.lowercased().filter { $0.isASCII && ($0.isLetter || $0.isNumber) }.prefix(32))
                    if cleaned != new { name = cleaned }
                }
            if let problem = nameProblem {
                Text(problem).font(.caption).foregroundStyle(.orange)
            } else {
                Text("Lowercase letters and digits. Your link will end in /\(name.isEmpty ? "name" : name)/mcp")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Picker("What is it?", selection: $kind) {
                ForEach(Kind.allCases) { Text($0.rawValue).tag($0) }
            }
            .pickerStyle(.segmented)
        }

        Section {
            switch kind {
            case .mcpURL:
                TextField("MCP endpoint", text: $url, prompt: Text("http://127.0.0.1:7879/mcp"))
                Text("The local address of an MCP server that speaks Streamable HTTP.").font(.caption).foregroundStyle(.secondary)
            case .stdio:
                TextField("Command", text: $command, prompt: Text("npx -y @modelcontextprotocol/server-everything"))
                    .font(.body.monospaced())
                TextField("Working folder (optional)", text: $cwd, prompt: Text("~/projects/my-server"))
                Label("The command runs as you, with your permissions. An allowed tool can do anything the command can.", systemImage: "exclamationmark.shield")
                    .font(.caption).foregroundStyle(.orange)
            case .http:
                TextField("Local address", text: $url, prompt: Text("http://127.0.0.1:8080"))
                TextField("Allowed paths (optional)", text: $paths, prompt: Text("/api, /files"))
                Text("Only these path prefixes are reachable. Leave empty to allow every path.").font(.caption).foregroundStyle(.secondary)
            }
        }

        if kind != .http {
            Section("Tools bots may use") {
                Picker("Start with", selection: $toolsDefault) {
                    ForEach(ToolsDefault.allCases) { Text($0.label).tag($0) }
                }
                Text("You can change each tool afterwards.").font(.caption).foregroundStyle(.secondary)
            }
        }

        Section("Your app's own login (optional)") {
            TextField("Header", text: $header, prompt: Text("Authorization: Bearer your-app-token"))
            Text("Added on this Mac before the call reaches your app. Kept in the Keychain; bots never see it.")
                .font(.caption).foregroundStyle(.secondary)
        }

        Section {
            if let failure = action.failure { ErrorBanner(failure: failure) { action.failure = nil } }
            HStack {
                if action.busy {
                    ProgressView().controlSize(.small)
                    Text(model.services.isEmpty ? "Setting up your relay on Cloudflare. The first time takes about 30 seconds." : "Updating your relay")
                        .font(.callout).foregroundStyle(.secondary)
                }
                Spacer()
                Button("Share", action: submit)
                    .buttonStyle(PressableStyle(prominent: true))
                    .keyboardShortcut(.defaultAction)
                    .disabled(!canSubmit || action.busy)
            }
        }
    }

    private var nameProblem: String? {
        if Self.reserved.contains(name) { return "\"\(name)\" is reserved. Pick another name." }
        if model.services.contains(where: { $0.name == name }) { return "You already share an app called \(name)." }
        return nil
    }

    private var canSubmit: Bool {
        guard !name.isEmpty, nameProblem == nil else { return false }
        switch kind {
        case .mcpURL, .http: return URL(string: url)?.host != nil
        case .stdio: return !(Self.split(command) ?? []).isEmpty
        }
    }

    private func submit() {
        let source: AppModel.NewService
        switch kind {
        case .mcpURL: source = .mcpURL(url.trimmingCharacters(in: .whitespaces))
        case .stdio:
            let dir = cwd.trimmingCharacters(in: .whitespaces)
            source = .stdio(command: Self.split(command) ?? [], cwd: dir.isEmpty ? nil : (dir as NSString).expandingTildeInPath)
        case .http:
            let list = paths.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
            source = .http(url.trimmingCharacters(in: .whitespaces), paths: list)
        }
        let service = name
        let header = header.trimmingCharacters(in: .whitespaces)
        let defaults = toolsDefault.rawValue
        // Pin the selection so the success screen stays put when the list reloads with the new app.
        model.destination = .add
        action.perform {
            let added = try await model.add(name: service, source: source, toolsDefault: defaults, header: header.isEmpty ? nil : header)
            addedName = service
            withAnimation(Motion.easeOut) { result = added }
        }
    }

    /// Splits a command line the way a POSIX shell does for the common cases: spaces separate words, single quotes
    /// keep everything literal, double quotes group words and honour \" and \\, a backslash escapes the next
    /// character outside quotes, and a leading ~ in an unquoted word is the home folder. Returns nil when a quote is
    /// left open or the line ends in a lone backslash.
    static func split(_ line: String) -> [String]? {
        var words: [String] = []
        var current = ""
        var inWord = false
        var quote: Character?
        var escaped = false
        var startsWithTilde = false
        let home = FileManager.default.homeDirectoryForCurrentUser.path

        func finish() {
            guard inWord else { return }
            if startsWithTilde, current == "~" || current.hasPrefix("~/") {
                current = home + current.dropFirst()
            }
            words.append(current)
            current = ""
            inWord = false
            startsWithTilde = false
        }

        for ch in line {
            if escaped {
                current.append(ch)
                escaped = false
                continue
            }
            switch quote {
            case "'":
                if ch == "'" { quote = nil } else { current.append(ch) }
            case "\"":
                if ch == "\"" { quote = nil }
                else if ch == "\\" { escaped = true }
                else { current.append(ch) }
            default:
                if ch == "\\" {
                    escaped = true
                    inWord = true
                } else if ch == "'" || ch == "\"" {
                    quote = ch
                    inWord = true
                } else if ch == " " || ch == "\t" {
                    finish()
                } else {
                    if !inWord { startsWithTilde = ch == "~" }
                    current.append(ch)
                    inWord = true
                }
            }
        }
        if quote != nil || escaped { return nil }
        finish()
        return words
    }

    // MARK: Success

    @ViewBuilder
    private func success(_ result: AddResult) -> some View {
        Section {
            Label("\(addedName) is shared", systemImage: "checkmark.circle.fill")
                .font(.title3.weight(.semibold))
                .foregroundStyle(.green)
            LabeledContent("Link") {
                HStack {
                    Text(result.url).textSelection(.enabled).font(.callout.monospaced()).lineLimit(1).truncationMode(.middle)
                    CopyButton(text: result.url, compact: true)
                }
            }
            if let sse = result.sseUrl {
                LabeledContent("SSE link (Qwen)") {
                    HStack {
                        Text(sse).textSelection(.enabled).font(.callout.monospaced()).lineLimit(1).truncationMode(.middle)
                        CopyButton(text: sse, compact: true)
                    }
                }
            }
            if let tools = result.tools, !tools.isEmpty {
                let allowed = tools.filter(\.allowed).count
                Text("\(allowed) of \(tools.count) tools allowed.").foregroundStyle(.secondary)
            }
            ForEach(result.warnings ?? [], id: \.self) { warning in
                Label(warning, systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange)
            }
        }
        Section {
            HStack {
                Button("Share another") {
                    withAnimation(Motion.easeOut) {
                        self.result = nil
                        name = ""
                        command = ""
                        header = ""
                    }
                }
                .buttonStyle(PressableStyle())
                Spacer()
                Button("Connect a bot") { model.destination = .service(addedName) }
                    .buttonStyle(PressableStyle(prominent: true))
                    .keyboardShortcut(.defaultAction)
            }
        }
    }
}
