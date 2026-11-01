// Health check (toprelay doctor) and settings.

import SwiftUI

struct HealthView: View {
    @Environment(AppModel.self) private var model
    @State private var result: DoctorResult?
    @State private var action = ActionState()

    var body: some View {
        Form {
            Section {
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Check everything").font(.headline)
                        Text("Your Cloudflare token, the relay, each app on this Mac, and whether bots can reach them.")
                            .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                    Spacer()
                    if action.busy { ProgressView().controlSize(.small) }
                    Button(result == nil ? "Run check" : "Run again") { run() }
                        .buttonStyle(PressableStyle(prominent: result == nil))
                        .disabled(action.busy)
                }
            }
            if let result {
                Section(result.ok ? "All good" : "Needs attention") {
                    ForEach(result.checks ?? []) { check in row(check) }
                }
            }
            if let failure = action.failure {
                Section { ErrorBanner(failure: failure) { action.failure = nil } }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Health check")
        .task { if result == nil { run() } }
    }

    private func run() {
        action.perform {
            let fresh = try await model.doctor()
            withAnimation(Motion.easeOut) { result = fresh }
        }
    }

    private func row(_ check: DoctorCheck) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: symbol(check.status))
                .foregroundStyle(color(check.status))
                .frame(width: 18)
            VStack(alignment: .leading, spacing: 2) {
                Text(check.name)
                if let detail = check.detail, !detail.isEmpty {
                    Text(detail).font(.caption).foregroundStyle(.secondary).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                }
                if let fix = check.fix, !fix.isEmpty, check.status != "ok" {
                    Text(fix).font(.caption).foregroundStyle(.orange).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                }
            }
        }
    }

    private func symbol(_ status: String) -> String {
        switch status {
        case "ok": "checkmark.circle.fill"
        case "warn": "exclamationmark.triangle.fill"
        case "fail": "xmark.octagon.fill"
        default: "minus.circle"
        }
    }

    private func color(_ status: String) -> Color {
        switch status {
        case "ok": .green
        case "warn": .orange
        case "fail": .red
        default: .secondary
        }
    }
}

struct SettingsView: View {
    @Environment(AppModel.self) private var model
    @State private var action = ActionState()
    @State private var version = ""

    var body: some View {
        Form {
            Section {
                Toggle(isOn: Binding(
                    get: { model.keepsRunning },
                    set: { on in action.perform { try await model.setKeepsRunning(on) } }
                )) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Keep apps reachable")
                        Text("Runs TopRelay in the background from login, even when this window and the menu are closed.")
                            .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
                .toggleStyle(.switch)
                .disabled(action.busy)
            }
            Section("Cloudflare") {
                if let list = model.list {
                    LabeledContent("Account", value: list.accountId)
                    LabeledContent("Relay", value: list.publicUrl.isEmpty ? "Not deployed yet" : list.publicUrl)
                }
            }
            Section("Files") {
                LabeledContent("Settings folder") {
                    Button("Show in Finder") {
                        NSWorkspace.shared.activateFileViewerSelecting([EnginePaths.configDirectory])
                    }
                    .buttonStyle(PressableStyle())
                }
                Text("Settings and call logs live here. Secrets are in your Keychain, not in this folder.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Section("About") {
                LabeledContent("App", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")
                LabeledContent("Engine", value: version)
                Link("Source code and docs", destination: URL(string: "https://github.com/harikanthl/TopRelay")!)
            }
            if let failure = action.failure {
                Section { ErrorBanner(failure: failure) { action.failure = nil } }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Settings")
        .task {
            struct Version: Decodable, Sendable { let version: String }
            if let engine = model.engine, let v: Version = try? await engine.run(["version"]) { version = v.version }
        }
    }
}
