// The menu-bar dropdown: a glance at status plus the two-click jobs (copy a link, pair a bot).
// It opens instantly, with no animation: people open it many times a day.

import SwiftUI

struct MenuBarView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow
    @State private var pairing: [String: PairResult] = [:]
    @State private var action = ActionState()

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            Divider().padding(.horizontal, 12)
            content
                .padding(.vertical, 6)
            if let failure = action.failure {
                ErrorBanner(failure: failure) { action.failure = nil }
                    .padding(.horizontal, 12)
                    .padding(.bottom, 8)
            }
            Divider().padding(.horizontal, 12)
            footer
        }
        .frame(width: 340)
        .task { model.readState() }
    }

    // MARK: Header

    private var header: some View {
        HStack(spacing: 8) {
            Text("TopRelay").font(.headline)
            Spacer()
            Text(summary)
                .font(.caption)
                .foregroundStyle(.secondary)
                .contentTransition(.opacity)
                .animation(Motion.easeOut, value: summary)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    private var summary: String {
        switch model.phase {
        case .loading: return "Loading…"
        case .engineMissing: return "Engine missing"
        case .needsLogin: return "Not set up"
        case .ready:
            switch model.health {
            case .noServices: return "No apps shared"
            case .engineStopped: return "Not running"
            case .allConnected: return model.services.count == 1 ? "Connected" : "All \(model.services.count) connected"
            case .someOffline:
                let up = model.services.filter { model.light(for: $0.name) == .connected }.count
                return "\(up) of \(model.services.count) connected"
            }
        }
    }

    // MARK: Content

    @ViewBuilder
    private var content: some View {
        switch model.phase {
        case .loading:
            ProgressView().controlSize(.small).frame(maxWidth: .infinity).padding(12)
        case .engineMissing:
            Text("TopRelay's engine is missing from the app. Reinstall TopRelay.")
                .font(.callout).padding(.horizontal, 14).padding(.vertical, 8)
        case .needsLogin:
            prompt(text: "Connect your Cloudflare account to give your apps a private address any AI can reach.",
                   button: "Set up TopRelay") { open(nil) }
        case .ready where model.services.isEmpty:
            prompt(text: "Share an app on this Mac (an MCP server or a local web app) with your AI tools.",
                   button: "Share an app") { open(.add) }
        case .ready:
            VStack(spacing: 2) {
                if model.health == .engineStopped { stoppedBanner }
                ForEach(model.services) { service in
                    row(service)
                    if let pair = pairing[service.name] {
                        PairCodeView(pair: pair)
                            .padding(.horizontal, 12)
                            .padding(.bottom, 6)
                    }
                }
                usage
            }
        }
    }

    private func prompt(text: String, button: String, perform: @escaping () -> Void) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(text).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Button(button, action: perform).buttonStyle(PressableStyle(prominent: true))
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 8)
    }

    private var stoppedBanner: some View {
        HStack(spacing: 8) {
            Image(systemName: "pause.circle.fill").foregroundStyle(.secondary)
            Text("TopRelay isn't running, so bots can't reach your apps.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 4)
            Button("Start") {
                action.perform { try await model.setKeepsRunning(true) }
            }
            .buttonStyle(PressableStyle(prominent: true))
            .disabled(action.busy)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 6)
    }

    private func row(_ service: ListedService) -> some View {
        let light = model.light(for: service.name)
        return HStack(spacing: 10) {
            StatusDot(light: light)
            VStack(alignment: .leading, spacing: 1) {
                Text(service.name).font(.body.weight(.medium))
                Text(light.label + (service.isMCP ? "" : " · web app"))
                    .font(.caption).foregroundStyle(.secondary)
                    .contentTransition(.opacity)
                    .animation(Motion.easeOut, value: light)
            }
            Spacer()
            CopyButton(text: service.url, label: "Copy link", compact: true)
            Menu {
                Button("Copy link") { model.copy(service.url) }
                if let sse = service.sseUrl { Button("Copy SSE link (Qwen)") { model.copy(sse) } }
                Button(pairing[service.name] == nil ? "Get a pairing code" : "Hide pairing code") { togglePair(service.name) }
                Divider()
                Button("Show details") { open(.service(service.name)) }
            } label: {
                Image(systemName: "ellipsis.circle").foregroundStyle(.secondary)
            }
            .menuStyle(.borderlessButton)
            .menuIndicator(.hidden)
            .fixedSize()
            .accessibilityLabel("More for \(service.name)")
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 6)
        .contentShape(Rectangle())
    }

    private func togglePair(_ service: String) {
        if pairing[service] != nil {
            pairing[service] = nil
            return
        }
        action.perform {
            let result = try await model.pair(service)
            pairing[service] = result
        }
    }

    @ViewBuilder
    private var usage: some View {
        if let status = model.status {
            VStack(alignment: .leading, spacing: 4) {
                ProgressView(value: min(status.percentOfLimit, 100), total: 100)
                    .progressViewStyle(.linear)
                    .tint(status.percentOfLimit > 80 ? .orange : .accentColor)
                Text("\(status.requestsToday.formatted()) of \(status.freeLimit.formatted()) free requests today")
                    .font(.caption).foregroundStyle(.secondary).monospacedDigit()
            }
            .padding(.horizontal, 14)
            .padding(.top, 8)
            .padding(.bottom, 4)
        }
    }

    // MARK: Footer

    private var footer: some View {
        HStack {
            Button("Open TopRelay") { open(model.destination) }
                .buttonStyle(QuietPressStyle())
                .keyboardShortcut("o")
            Spacer()
            Button("Quit") { NSApp.terminate(nil) }
                .buttonStyle(QuietPressStyle())
                .keyboardShortcut("q")
                .help("Bots can still reach your apps after you quit, if “Keep apps reachable” is on in Settings.")
        }
        .font(.callout)
        .padding(.horizontal, 14)
        .padding(.vertical, 9)
    }

    private func open(_ destination: Destination?) {
        if let destination { model.destination = destination }
        openWindow(id: "main")
        NSApp.activate()
    }
}
