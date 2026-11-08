import SwiftUI
import TopRelayKit

/// A "Reach from anywhere" switch for an app's settings: on registers the app's local MCP server with TopRelay
/// (the credential read from the app's own token file), shows the public URL and whether it's connected, and
/// makes pairing codes. Off removes the service, which revokes every bot's access.
///
/// ```swift
/// ReachFromAnywhereToggle(model: .init(service: "notes", mcpURL: "http://127.0.0.1:7879/mcp",
///     credential: .file(header: "X-Notes-Token", path: tokenFile.path)))
/// ```
@MainActor
public final class ReachFromAnywhereModel: ObservableObject {
    @Published public private(set) var isOn = false
    @Published public private(set) var busy = false
    /// The position the switch was just moved to, while that change is still being made (nil otherwise). The
    /// switch shows it at once; if the change fails it goes back.
    @Published public private(set) var pendingOn: Bool?
    @Published public private(set) var url: String?
    @Published public private(set) var connected = false
    @Published public private(set) var lastError: String?
    @Published public private(set) var requests: Int64 = 0
    @Published public private(set) var pairing: PairingCode?
    @Published public var failure: TopRelayError?

    public let service: String
    let mcpURL: String
    let credential: Credential?
    let relay: TopRelay
    // nonisolated(unsafe): deinit (not on the main actor) cancels it; it's only replaced on the main actor.
    nonisolated(unsafe) private var watch: Task<Void, Never>?

    deinit {
        watch?.cancel() // ends the reading thread and its socket
    }

    public init(service: String, mcpURL: String, credential: Credential?, relay: TopRelay = TopRelay()) {
        self.service = service
        self.mcpURL = mcpURL
        self.credential = credential
        self.relay = relay
    }

    /// Reads whether the service exists and starts following its status.
    public func load() async {
        do {
            let svc = try await relay.services().first { $0.name == service }
            isOn = svc != nil
            url = svc?.url
            if isOn { follow() }
        } catch let e as TopRelayError {
            failure = e
        } catch {}
    }

    public func setOn(_ on: Bool) async {
        busy = true
        pendingOn = on
        defer {
            busy = false
            pendingOn = nil
        }
        do {
            if on {
                let exists = try await relay.services().contains { $0.name == service }
                if exists {
                    // Already registered: refresh only the credential (live, keeps every bot's access).
                    try await relay.setCredential(service: service, credential)
                } else {
                    url = try await relay.register(service: service, mcpURL: mcpURL, credential: credential).url
                }
                if url == nil { url = try await relay.services().first { $0.name == service }?.url }
                isOn = true
                follow()
            } else {
                try await relay.remove(service: service)
                watch?.cancel()
                isOn = false
                url = nil
                connected = false
                pairing = nil
            }
        } catch let e as TopRelayError {
            failure = e
        } catch {
            failure = TopRelayError(code: "error", message: error.localizedDescription)
        }
    }

    public func makePairingCode() async {
        do {
            pairing = try await relay.pairingCode(service: service)
        } catch let e as TopRelayError {
            failure = e
        } catch {}
    }

    /// Follows the agent's state; when the stream ends (the agent stopped or restarted), shows that and tries
    /// again every few seconds.
    private func follow() {
        watch?.cancel()
        let service = self.service
        let relay = self.relay
        watch = Task { [weak self] in
            while !Task.isCancelled {
                var problem = "TopRelay isn't running"
                do {
                    for try await state in relay.stateUpdates() {
                        let s = state.running ? state.services[service] : nil
                        await MainActor.run {
                            self?.connected = s?.connected ?? false
                            self?.lastError = state.running ? (s?.lastError.isEmpty == false ? s?.lastError : nil) : "TopRelay isn't running"
                            self?.requests = s?.requests ?? 0
                        }
                    }
                } catch {
                    problem = (error as? TopRelayError)?.errorDescription ?? error.localizedDescription
                }
                guard !Task.isCancelled, self != nil else { return }
                await MainActor.run {
                    self?.connected = false
                    self?.lastError = problem
                }
                try? await Task.sleep(nanoseconds: 3_000_000_000)
            }
        }
    }
}

public struct ReachFromAnywhereToggle: View {
    @ObservedObject var model: ReachFromAnywhereModel
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var copied = false
    @State private var copiedReset: Task<Void, Never>?

    public init(model: ReachFromAnywhereModel) {
        self.model = model
    }

    /// Shown and hidden content fades; it rises a few points unless the user asked for less motion.
    private var reveal: AnyTransition {
        reduceMotion ? .opacity : .opacity.combined(with: .offset(y: 4))
    }

    private var shownOn: Bool { model.pendingOn ?? model.isOn }

    public var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Toggle("Reach from anywhere", isOn: Binding(get: { shownOn }, set: { on in Task { await model.setOn(on) } }))
                    .disabled(model.busy)
                if model.busy {
                    ProgressView().controlSize(.small)
                        .transition(.opacity)
                }
            }
            Group {
                if model.pendingOn == true {
                    Text("Setting up the relay… the first time can take a minute.")
                } else if model.pendingOn == false {
                    Text("Turning off…")
                } else {
                    Text("Lets claude.ai, ChatGPT and other bots you approve use this app's tools, through a relay on your own Cloudflare account.")
                }
            }
            .font(.caption)
            .foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)

            if model.isOn, let url = model.url {
                VStack(alignment: .leading, spacing: 8) {
                    status
                    HStack {
                        Text(url)
                            .font(.system(.caption, design: .monospaced))
                            .lineLimit(1)
                            .truncationMode(.middle)
                            .textSelection(.enabled)
                            .help(url)
                        Button { copy(url) } label: {
                            Label(copied ? "Copied" : "Copy", systemImage: copied ? "checkmark" : "doc.on.doc")
                                .contentTransition(.opacity)
                        }
                        .accessibilityLabel(copied ? "Copied the address" : "Copy the address")
                    }
                    HStack(spacing: 8) {
                        Button("Get pairing code") { Task { await model.makePairingCode() } }
                        if let p = model.pairing {
                            Text(p.code)
                                .font(.system(.body, design: .monospaced))
                                .textSelection(.enabled)
                            Text("for claude.ai, valid until \(p.expiresAt.formatted(date: .omitted, time: .shortened))")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                    .animation(.easeOut(duration: 0.2), value: model.pairing?.code)
                }
                .transition(reveal)
            }
        }
        .animation(.easeOut(duration: 0.2), value: model.isOn && model.url != nil)
        .animation(.easeOut(duration: 0.15), value: model.busy)
        .task { await model.load() }
        .alert(model.failure?.message ?? "", isPresented: Binding(get: { model.failure != nil }, set: { if !$0 { model.failure = nil } })) {
            Button("OK") { model.failure = nil }
        } message: {
            Text(model.failure?.fix ?? "")
        }
    }

    private var status: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Circle()
                .fill(model.connected ? Color.green : Color.orange)
                .frame(width: 8, height: 8)
                .accessibilityHidden(true) // the text says it
            Text(model.connected ? "Connected" : (model.lastError ?? "Connecting…"))
                .font(.caption)
                .lineLimit(2)
                .help(model.lastError ?? "")
            Spacer()
            Text("\(model.requests) calls")
                .font(.caption)
                .monospacedDigit()
                .contentTransition(.numericText())
                .foregroundStyle(.secondary)
        }
        .animation(.easeOut(duration: 0.15), value: model.connected)
        .animation(.easeOut(duration: 0.15), value: model.requests)
    }

    private func copy(_ url: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(url, forType: .string)
        withAnimation(.easeOut(duration: 0.15)) { copied = true }
        copiedReset?.cancel()
        copiedReset = Task {
            try? await Task.sleep(nanoseconds: 1_500_000_000)
            guard !Task.isCancelled else { return }
            withAnimation(.easeOut(duration: 0.2)) { copied = false }
        }
    }
}
