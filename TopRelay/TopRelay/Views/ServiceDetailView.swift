// One shared app: its link, how to connect each bot, which tools are allowed, recent calls, and management.

import SwiftUI

struct ServiceDetailView: View {
    enum Tab: String, CaseIterable, Identifiable {
        case connect = "Connect", tools = "Tools", activity = "Activity", manage = "Manage"
        var id: String { rawValue }
    }

    let service: ListedService
    @Environment(AppModel.self) private var model
    @State private var tab: Tab = .connect

    private var tabs: [Tab] { service.isMCP ? Tab.allCases : [.connect, .activity, .manage] }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
                .padding(.horizontal, 24)
                .padding(.top, 20)
                .padding(.bottom, 12)
            Picker("Section", selection: $tab) {
                ForEach(tabs) { Text($0.rawValue).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(.horizontal, 24)
            .padding(.bottom, 8)
            Divider()
            // Switching tabs is frequent: no animation.
            switch tab {
            case .connect: ConnectTab(service: service)
            case .tools: ToolsTab(service: service)
            case .activity: ActivityTab(service: service)
            case .manage: ManageTab(service: service)
            }
        }
        .navigationTitle(service.name)
    }

    private var header: some View {
        let light = model.light(for: service.name)
        let local = model.localState(for: service.name)
        return VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                StatusDot(light: light, size: 10)
                Text(light.label)
                    .foregroundStyle(.secondary)
                    .contentTransition(.opacity)
                    .animation(Motion.easeOut, value: light)
                if let error = local?.lastError, !error.isEmpty, light != .connected {
                    Text("· \(error)").foregroundStyle(.secondary).lineLimit(1).truncationMode(.tail)
                }
                Spacer()
                if let remote = model.remoteStatus(for: service.name) {
                    Text("\(remote.requests.formatted()) calls today · \(remote.rejected.formatted()) refused")
                        .font(.caption).foregroundStyle(.secondary).monospacedDigit()
                }
            }
            .font(.callout)
            HStack(spacing: 8) {
                Text(service.url)
                    .font(.callout.monospaced())
                    .textSelection(.enabled)
                    .lineLimit(1)
                    .truncationMode(.middle)
                CopyButton(text: service.url, label: "Copy link")
            }
            Text(service.source).font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
        }
    }
}

// MARK: - Connect

private struct ConnectTab: View {
    private enum Bot: String, CaseIterable, Identifiable {
        case claudeAI = "claude.ai"
        case chatGPT = "ChatGPT"
        case claudeAPI = "Claude API"
        case openAI = "OpenAI API"
        case xai = "Grok (xAI API)"
        case gemini = "Gemini API"
        case qwen = "Qwen (Model Studio)"
        case curl = "Anything else"
        var id: String { rawValue }

        var usesOAuth: Bool { self == .claudeAI || self == .chatGPT }
        /// The engine's snippet key for `toprelay key --client`.
        var snippetKey: String? {
            switch self {
            case .claudeAPI: "claude-api"
            case .openAI: "openai"
            case .xai: "xai"
            case .gemini: "gemini"
            case .qwen: "qwen"
            case .curl: "curl"
            case .claudeAI, .chatGPT: nil
            }
        }
    }

    private enum Lifetime: String, CaseIterable, Identifiable {
        case hour = "1h", day = "24h", month = "720h", never = ""
        var id: String { rawValue }
        var label: String {
            switch self {
            case .hour: "1 hour"
            case .day: "1 day"
            case .month: "30 days"
            case .never: "Until revoked"
            }
        }
    }

    let service: ListedService
    @Environment(AppModel.self) private var model
    @State private var bot: Bot = .claudeAI
    @State private var pair: PairResult?
    @State private var key: KeyResult?
    @State private var lifetime: Lifetime = .month
    @State private var restrictTools = false
    @State private var chosenTools: Set<String> = []
    @State private var action = ActionState()

    private var bots: [Bot] { service.isMCP ? Bot.allCases : [.curl] }

    var body: some View {
        Form {
            Section {
                Picker("Bot", selection: $bot) {
                    ForEach(bots) { Text($0.rawValue).tag($0) }
                }
                .onChange(of: bot) { key = nil }
            }
            if bot.usesOAuth { oauthSteps } else { keySteps }
            if let failure = action.failure {
                Section { ErrorBanner(failure: failure) { action.failure = nil } }
            }
        }
        .formStyle(.grouped)
        .onAppear { if !service.isMCP { bot = .curl } }
    }

    @ViewBuilder
    private var oauthSteps: some View {
        Section("Steps") {
            step(1, bot == .claudeAI
                 ? "In claude.ai, open Settings → Connectors → Add custom connector."
                 : "In ChatGPT, open Settings → Apps → Create (developer mode), or your workspace's connector settings.")
            step(2, "Paste this app's link as the server URL.")
            if bot == .claudeAI { step(3, "For the OAuth client, choose “Use Claude's published identity”.") }
            step(bot == .claudeAI ? 4 : 3, "When the TopRelay page asks for a pairing code, enter the one below.")
        }
        Section("Pairing code") {
            if let pair {
                PairCodeView(pair: pair)
            }
            HStack {
                Text(pair == nil ? "Codes work once and expire after 10 minutes." : "Need another one?")
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button(pair == nil ? "Get a pairing code" : "New code") {
                    action.perform { pair = try await model.pair(service.name) }
                }
                .buttonStyle(PressableStyle(prominent: pair == nil))
                .disabled(action.busy)
            }
        }
    }

    @ViewBuilder
    private var keySteps: some View {
        Section("Key") {
            Picker("Valid for", selection: $lifetime) {
                ForEach(Lifetime.allCases) { Text($0.label).tag($0) }
            }
            if service.isMCP {
                Toggle("Only some tools", isOn: $restrictTools)
                if restrictTools {
                    ForEach(service.allowedTools ?? [], id: \.self) { tool in
                        Toggle(tool, isOn: Binding(
                            get: { chosenTools.contains(tool) },
                            set: { on in if on { chosenTools.insert(tool) } else { chosenTools.remove(tool) } }
                        ))
                        .toggleStyle(.checkbox)
                    }
                }
            }
            HStack {
                Text("The key is shown once. TopRelay keeps only its id, so you can revoke it.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                Spacer()
                Button(key == nil ? "Create key" : "Create another") {
                    let tools = restrictTools ? Array(chosenTools).sorted() : []
                    action.perform {
                        let minted = try await model.mintKey(service.name, tools: tools, validFor: lifetime.rawValue)
                        withAnimation(Motion.easeOut) { key = minted }
                    }
                }
                .buttonStyle(PressableStyle(prominent: key == nil))
                .disabled(action.busy || (restrictTools && chosenTools.isEmpty))
            }
        }
        if let key {
            Section("Use it") {
                LabeledContent("Key") {
                    HStack {
                        Text(key.key).font(.caption.monospaced()).lineLimit(1).truncationMode(.middle).textSelection(.enabled)
                        CopyButton(text: key.key, compact: true)
                    }
                }
                if let snippetKey = bot.snippetKey, let snippet = key.snippets?[snippetKey] {
                    VStack(alignment: .leading, spacing: 6) {
                        HStack {
                            Text("Example").font(.caption).foregroundStyle(.secondary)
                            Spacer()
                            CopyButton(text: snippet, label: "Copy example")
                        }
                        ScrollView(.horizontal) {
                            Text(snippet).font(.caption.monospaced()).textSelection(.enabled).padding(8)
                        }
                        .background(RoundedRectangle(cornerRadius: 6, style: .continuous).fill(.quaternary.opacity(0.5)))
                    }
                }
                if let expires = key.expires {
                    Text("Expires \(Date(timeIntervalSince1970: TimeInterval(expires)), style: .date). Key id \(key.id).")
                        .font(.caption).foregroundStyle(.secondary)
                } else {
                    Text("Never expires. Key id \(key.id).").font(.caption).foregroundStyle(.secondary)
                }
                ForEach(key.warnings ?? [], id: \.self) { Label($0, systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange) }
            }
        }
    }

    private func step(_ n: Int, _ text: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Text("\(n)").font(.caption.weight(.semibold)).frame(width: 18, height: 18).background(Circle().fill(.quaternary))
            Text(text).fixedSize(horizontal: false, vertical: true)
        }
    }
}

// MARK: - Tools

private struct ToolsTab: View {
    let service: ListedService
    @Environment(AppModel.self) private var model
    @State private var result: ToolsResult?
    @State private var action = ActionState()
    @State private var loading = true

    var body: some View {
        Form {
            if loading && result == nil {
                Section { HStack { ProgressView().controlSize(.small); Text("Asking \(service.name) for its tools…").foregroundStyle(.secondary) } }
            }
            if let result {
                if !result.reachable {
                    Section {
                        Label("\(service.name) isn't answering on this Mac\(result.error.map { ": \($0)" } ?? ""). Showing your saved choices.", systemImage: "bolt.horizontal.circle")
                            .font(.callout).foregroundStyle(.secondary)
                    }
                }
                let tools = result.tools ?? []
                let allowed = tools.filter(\.allowed).count
                Section {
                    ForEach(tools) { tool in toolRow(tool) }
                } header: {
                    Text("\(allowed) of \(tools.count) allowed")
                } footer: {
                    VStack(alignment: .leading, spacing: 4) {
                        if allowed > 20 {
                            Label("More than 20 tools makes most AI models pick worse. Allow only what you need.", systemImage: "exclamationmark.triangle")
                                .foregroundStyle(.orange)
                        }
                        Text("New tools the app adds later are \(result.policy.default == "allow" ? "allowed" : "hidden") until you decide.")
                            .foregroundStyle(.secondary)
                    }
                    .font(.caption)
                }
            }
            if let failure = action.failure {
                Section { ErrorBanner(failure: failure) { action.failure = nil } }
            }
        }
        .formStyle(.grouped)
        .task { await load() }
    }

    private func toolRow(_ tool: ToolView) -> some View {
        Toggle(isOn: Binding(
            get: { tool.allowed },
            set: { on in
                action.perform {
                    let updated = try await model.setTool(service.name, tool: tool.name, allowed: on)
                    result = updated
                }
            }
        )) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(tool.title ?? tool.name).font(.body.weight(.medium))
                    if tool.readOnly { badge("read-only", .green) }
                    if tool.destructive { badge("can delete", .red) }
                    if tool.openWorld { badge("reaches the internet", .blue) }
                }
                if let description = tool.description, !description.isEmpty {
                    Text(description).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
                ForEach(tool.warnings ?? [], id: \.self) { Text($0).font(.caption2).foregroundStyle(.orange) }
            }
        }
        .toggleStyle(.switch)
        .disabled(action.busy)
    }

    private func badge(_ text: String, _ color: Color) -> some View {
        Text(text)
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 1)
            .background(Capsule().fill(color.opacity(0.15)))
            .foregroundStyle(color)
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            result = try await model.tools(service.name)
        } catch {
            action.failure = error.asFailure
        }
    }
}

// MARK: - Activity

private struct ActivityTab: View {
    let service: ListedService
    @State private var calls: [CallRecord] = []

    var body: some View {
        Group {
            if calls.isEmpty {
                ContentUnavailableView("No calls yet", systemImage: "list.bullet.rectangle",
                                       description: Text("Calls from bots appear here. Only the time, tool and result are kept; never what was sent."))
            } else {
                Table(calls) {
                    TableColumn("Time") { call in
                        if let date = call.date { Text(date, style: .time).monospacedDigit() } else { Text(call.time) }
                    }
                    .width(min: 70, ideal: 80)
                    TableColumn("Call") { call in Text(call.label).lineLimit(1) }
                    TableColumn("Result") { call in
                        if call.wasRefused {
                            Label("Refused", systemImage: "hand.raised").foregroundStyle(.orange)
                        } else {
                            Text(call.status < 400 ? "OK" : "\(call.status)")
                                .foregroundStyle(call.status < 400 ? AnyShapeStyle(.secondary) : AnyShapeStyle(.red))
                        }
                    }
                    .width(min: 70, ideal: 90)
                    TableColumn("Time taken") { call in Text("\(call.durationMs) ms").monospacedDigit().foregroundStyle(.secondary) }
                        .width(min: 70, ideal: 90)
                    TableColumn("Client") { call in Text(call.client ?? "").foregroundStyle(.secondary).lineLimit(1) }
                }
            }
        }
        .task(id: service.name) {
            // The log is a local file; re-read it every couple of seconds while this tab is open.
            while !Task.isCancelled {
                let name = service.name
                let fresh = await Task.detached { AppModel.recentCalls(service: name) }.value
                if fresh != calls { calls = fresh }
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }
}

// MARK: - Manage

private struct ManageTab: View {
    let service: ListedService
    @Environment(AppModel.self) private var model
    @State private var test: TestResult?
    @State private var rotated: RotateResult?
    @State private var revokeID = ""
    @State private var revoked: KeyRevokeResult?
    @State private var confirmRotate = false
    @State private var confirmRemove = false
    @State private var action = ActionState()

    var body: some View {
        Form {
            Section("Check the connection") {
                HStack {
                    Text("Makes one harmless call through your relay and measures the delay it adds.")
                        .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    Spacer()
                    if action.busy { ProgressView().controlSize(.small) }
                    Button("Test") { action.perform { test = try await model.test(service.name) } }
                        .buttonStyle(PressableStyle())
                        .disabled(action.busy)
                }
                if let test {
                    Label(test.ok ? "Working" : "Failed\(test.status.map { " (HTTP \($0))" } ?? "")",
                          systemImage: test.ok ? "checkmark.circle.fill" : "xmark.octagon.fill")
                        .foregroundStyle(test.ok ? .green : .red)
                    LabeledContent("Through the relay", value: String(format: "%.1f ms", test.remoteMs))
                    LabeledContent("Direct on this Mac", value: String(format: "%.1f ms", test.localMs))
                    LabeledContent("Added by the relay", value: String(format: "%.1f ms", test.relayAddedMs))
                }
            }

            Section("Revoke a key") {
                TextField("Key id", text: $revokeID, prompt: Text("16 characters, shown when the key was made"))
                    .font(.body.monospaced())
                HStack {
                    if let revoked { Text("Revoked \(revoked.id).").font(.caption).foregroundStyle(.secondary) }
                    Spacer()
                    Button("Revoke") {
                        let id = revokeID.trimmingCharacters(in: .whitespaces).lowercased()
                        action.perform {
                            revoked = try await model.revokeKey(service.name, id: id)
                            revokeID = ""
                        }
                    }
                    .buttonStyle(PressableStyle())
                    .disabled(revokeID.trimmingCharacters(in: .whitespaces).count != 16 || action.busy)
                }
            }

            Section("Start over") {
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Disconnect every bot")
                        Text("Invalidates all keys and sign-ins for \(service.name). Bots must connect again.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Disconnect all") { confirmRotate = true }.buttonStyle(PressableStyle())
                }
                if let rotated { Text("Done. \(rotated.oauthRevoked) sign-ins revoked.").font(.caption).foregroundStyle(.secondary) }
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Stop sharing \(service.name)")
                        Text("Removes its link and revokes everything. The app on your Mac isn't touched.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Stop sharing", role: .destructive) { confirmRemove = true }.buttonStyle(PressableStyle())
                }
            }

            if let failure = action.failure {
                Section { ErrorBanner(failure: failure) { action.failure = nil } }
            }
        }
        .formStyle(.grouped)
        .confirmationDialog("Disconnect every bot from \(service.name)?", isPresented: $confirmRotate) {
            Button("Disconnect all", role: .destructive) { action.perform { rotated = try await model.rotate(service.name) } }
        } message: {
            Text("All keys and sign-ins stop working at once. Your relay is redeployed, which takes a few seconds.")
        }
        .confirmationDialog("Stop sharing \(service.name)?", isPresented: $confirmRemove) {
            Button("Stop sharing", role: .destructive) {
                action.perform {
                    try await model.remove(service.name)
                    model.destination = model.services.first.map { .service($0.name) } ?? .add
                }
            }
        } message: {
            Text("Its link stops working and every bot loses access.")
        }
    }
}
