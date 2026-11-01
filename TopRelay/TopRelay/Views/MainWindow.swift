// The window for jobs that don't fit a dropdown: setup, adding an app, tools, keys, activity, health.
// While it's open the app shows in the Dock and app switcher; when it closes, TopRelay goes back to the menu bar.

import SwiftUI

struct MainWindow: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Group {
            switch model.phase {
            case .loading:
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            case .engineMissing:
                ContentUnavailableView(
                    "TopRelay's engine is missing",
                    systemImage: "exclamationmark.triangle",
                    description: Text("The app bundle is incomplete. Download TopRelay again.")
                )
            case .needsLogin:
                SetupView()
            case .ready:
                ReadyView()
            case .failed(let failure):
                VStack(spacing: 14) {
                    ErrorBanner(failure: failure)
                    Button("Try again") { Task { await model.reload() } }
                        .buttonStyle(PressableStyle(prominent: true))
                }
                .frame(maxWidth: 440)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .frame(minWidth: 760, minHeight: 520)
        .onAppear { NSApp.setActivationPolicy(.regular) }
        .onDisappear { NSApp.setActivationPolicy(.accessory) }
    }
}

private struct ReadyView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        @Bindable var model = model
        NavigationSplitView {
            List(selection: $model.destination) {
                Section("Apps") {
                    ForEach(model.services) { service in
                        HStack(spacing: 8) {
                            StatusDot(light: model.light(for: service.name))
                            Text(service.name)
                            Spacer()
                            if !service.isMCP {
                                Text("web").font(.caption2).foregroundStyle(.secondary)
                            }
                        }
                        .tag(Destination.service(service.name))
                    }
                    Label("Share an app", systemImage: "plus")
                        .tag(Destination.add)
                }
                Section {
                    Label("Health check", systemImage: "stethoscope").tag(Destination.health)
                    Label("Settings", systemImage: "gearshape").tag(Destination.settings)
                }
            }
            .navigationSplitViewColumnWidth(min: 190, ideal: 210)
        } detail: {
            switch model.destination {
            case .service(let name):
                if let service = model.services.first(where: { $0.name == name }) {
                    ServiceDetailView(service: service).id(service.name)
                } else {
                    ContentUnavailableView("That app was removed", systemImage: "questionmark.app")
                }
            case .add:
                AddServiceView()
            case .health:
                HealthView()
            case .settings:
                SettingsView()
            case nil:
                if model.services.isEmpty {
                    AddServiceView()
                } else {
                    ContentUnavailableView("Pick an app", systemImage: "sidebar.left", description: Text("Choose an app on the left to see its link, tools and activity."))
                }
            }
        }
        .onAppear {
            if model.destination == nil, let first = model.services.first { model.destination = .service(first.name) }
        }
    }
}

// MARK: - Setup

/// First run: what TopRelay does, then a Cloudflare API token. Steps slide in the direction you move.
struct SetupView: View {
    static let tokenURL = URL(string: "https://dash.cloudflare.com/profile/api-tokens?permissionGroupKeys=%5B%7B%22key%22:%22workers_scripts%22,%22type%22:%22edit%22%7D,%7B%22key%22:%22workers_kv_storage%22,%22type%22:%22edit%22%7D,%7B%22key%22:%22account_settings%22,%22type%22:%22read%22%7D%5D&name=TopRelay&accountId=*&zoneId=all")!

    private enum Step: Int { case welcome, token, account, done }

    @Environment(AppModel.self) private var model
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var step: Step = .welcome
    @State private var forward = true
    @State private var token = ""
    @State private var accounts: [AccountChoice] = []
    @State private var account: String?
    @State private var signedIn: LoginResult?
    @State private var action = ActionState()

    var body: some View {
        VStack(spacing: 0) {
            ZStack {
                switch step {
                case .welcome: welcome.transition(slide)
                case .token: tokenStep.transition(slide)
                case .account: accountStep.transition(slide)
                case .done: doneStep.transition(slide)
                }
            }
            .frame(maxWidth: 460, maxHeight: .infinity)
            .padding(32)
            if let failure = action.failure {
                ErrorBanner(failure: failure) { action.failure = nil }
                    .frame(maxWidth: 460)
                    .padding(.horizontal, 32)
                    .padding(.bottom, 24)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var slide: AnyTransition {
        if reduceMotion { return .opacity }
        let offset: CGFloat = 24
        return .asymmetric(
            insertion: .opacity.combined(with: .offset(x: forward ? offset : -offset)),
            removal: .opacity.combined(with: .offset(x: forward ? -offset : offset))
        )
    }

    private func go(_ next: Step) {
        forward = next.rawValue > step.rawValue
        withAnimation(reduceMotion ? Motion.easeOut : Motion.step) { step = next }
    }

    private var welcome: some View {
        VStack(alignment: .leading, spacing: 16) {
            Image(systemName: "antenna.radiowaves.left.and.right")
                .font(.system(size: 34, weight: .medium))
                .foregroundStyle(.tint)
            Text("Give your apps an address any AI can reach")
                .font(.title2.weight(.semibold))
            VStack(alignment: .leading, spacing: 10) {
                bullet("lock.shield", "Runs on your own free Cloudflare account. Nothing goes through us.")
                bullet("checklist", "You choose which tools each bot may use.")
                bullet("wifi", "Works behind any router. Nothing to open on your network.")
                bullet("eye", "Cloudflare handles the traffic on your account, so it isn't end-to-end encrypted.")
            }
            Button("Get started") { go(.token) }
                .buttonStyle(PressableStyle(prominent: true))
                .keyboardShortcut(.defaultAction)
                .padding(.top, 6)
        }
    }

    private func bullet(_ symbol: String, _ text: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Image(systemName: symbol).foregroundStyle(.secondary).frame(width: 18)
            Text(text).fixedSize(horizontal: false, vertical: true)
        }
    }

    private var tokenStep: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Connect Cloudflare").font(.title2.weight(.semibold))
            Text("Create an API token with the right permissions. The link fills them in for you: Workers Scripts (Edit), Workers KV Storage (Edit), Account Settings (Read).")
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            Link(destination: Self.tokenURL) {
                Label("Create a token on Cloudflare", systemImage: "arrow.up.right.square")
            }
            SecureField("Paste the token here", text: $token)
                .textFieldStyle(.roundedBorder)
                .onSubmit(signIn)
            Text("Stored in your Mac's Keychain. Only used to deploy and update your relay.")
                .font(.caption).foregroundStyle(.secondary)
            HStack {
                Button("Back") { go(.welcome) }.buttonStyle(PressableStyle())
                Spacer()
                if action.busy { ProgressView().controlSize(.small) }
                Button("Continue", action: signIn)
                    .buttonStyle(PressableStyle(prominent: true))
                    .keyboardShortcut(.defaultAction)
                    .disabled(token.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || action.busy)
            }
        }
    }

    private func signIn() {
        let token = token.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !token.isEmpty else { return }
        action.perform {
            do {
                signedIn = try await model.login(token: token)
                self.token = ""
                go(.done)
            } catch let failure as EngineFailure where failure.code == "choose_account" {
                accounts = failure.accounts ?? []
                account = accounts.first?.id
                go(.account)
            }
        }
    }

    private var accountStep: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Choose an account").font(.title2.weight(.semibold))
            Text("This token can see several Cloudflare accounts. The relay will be deployed to the one you pick.")
                .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Picker("Account", selection: $account) {
                ForEach(accounts) { Text($0.name).tag(Optional($0.id)) }
            }
            .pickerStyle(.radioGroup)
            .labelsHidden()
            HStack {
                Button("Back") { go(.token) }.buttonStyle(PressableStyle())
                Spacer()
                if action.busy { ProgressView().controlSize(.small) }
                Button("Continue") {
                    let token = token.trimmingCharacters(in: .whitespacesAndNewlines)
                    action.perform {
                        signedIn = try await model.login(token: token, account: account)
                        self.token = ""
                        go(.done)
                    }
                }
                .buttonStyle(PressableStyle(prominent: true))
                .keyboardShortcut(.defaultAction)
                .disabled(account == nil || action.busy)
            }
        }
    }

    private var doneStep: some View {
        VStack(alignment: .leading, spacing: 14) {
            Image(systemName: "checkmark.circle.fill").font(.system(size: 34)).foregroundStyle(.green)
            Text("Connected to \(signedIn?.account.name ?? "Cloudflare")").font(.title2.weight(.semibold))
            Text("Next, share an app. TopRelay deploys your relay the first time, which takes about half a minute.")
                .foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Button("Share an app") {
                model.destination = .add
                token = ""
                Task { await model.reload() }
            }
            .buttonStyle(PressableStyle(prominent: true))
            .keyboardShortcut(.defaultAction)
        }
    }
}
