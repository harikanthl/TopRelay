// TopRelay: a menu-bar app with one window, driving the bundled `toprelay` engine.

import SwiftUI

@main
struct TopRelayApp: App {
    @State private var model = AppModel()

    init() {
        // Writing a token to an engine that already exited must fail with an error, not kill the app.
        signal(SIGPIPE, SIG_IGN)
    }

    var body: some Scene {
        MenuBarExtra {
            MenuBarView()
                .environment(model)
        } label: {
            MenuBarIcon(health: model.phase == .ready ? model.health : .noServices)
                .task { model.start() }
        }
        .menuBarExtraStyle(.window)

        Window("TopRelay", id: "main") {
            MainWindow()
                .environment(model)
                .task { model.start() }
        }
        .windowResizability(.contentMinSize)
        .defaultSize(width: 880, height: 600)
    }
}

/// The menu-bar symbol: full antenna when everything is reachable, a warning when something is offline,
/// and a struck-through antenna when nothing is running.
private struct MenuBarIcon: View {
    let health: AppModel.Health

    var body: some View {
        Image(systemName: symbol)
            .accessibilityLabel(label)
    }

    private var symbol: String {
        switch health {
        case .allConnected: "antenna.radiowaves.left.and.right"
        case .someOffline: "exclamationmark.triangle"
        case .engineStopped, .noServices: "antenna.radiowaves.left.and.right.slash"
        }
    }

    private var label: String {
        switch health {
        case .allConnected: "TopRelay: all apps reachable"
        case .someOffline: "TopRelay: some apps offline"
        case .engineStopped: "TopRelay: not running"
        case .noServices: "TopRelay"
        }
    }
}
