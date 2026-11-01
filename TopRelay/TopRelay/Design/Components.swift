// Shared look and motion. Rules (emil-design-eng):
// - Frequent things don't animate (opening the menu). State changes crossfade, ~200 ms, no movement.
// - Every pressable thing scales to 0.97 on press, fast. Exits are faster than entrances.
// - Strong ease-out curves, nothing over 300 ms, nothing from scale 0, and no movement with Reduce Motion.

import SwiftUI

enum Motion {
    /// Strong ease-out: cubic-bezier(0.23, 1, 0.32, 1).
    static let easeOut = Animation.timingCurve(0.23, 1, 0.32, 1, duration: 0.2)
    static let press = Animation.timingCurve(0.23, 1, 0.32, 1, duration: 0.12)
    static let release = Animation.timingCurve(0.23, 1, 0.32, 1, duration: 0.16)
    static let reveal = Animation.timingCurve(0.23, 1, 0.32, 1, duration: 0.24)
    /// Barely-bouncy spring for rare, spatial moves (setup steps).
    static let step = Animation.spring(duration: 0.35, bounce: 0.12)
}

// MARK: Status

enum ServiceLight: Equatable {
    case connected, connecting, offline, stopped

    var color: Color {
        switch self {
        case .connected: .green
        case .connecting: .orange
        case .offline: .red
        case .stopped: .secondary
        }
    }

    var label: String {
        switch self {
        case .connected: "Connected"
        case .connecting: "Reconnecting…"
        case .offline: "Offline"
        case .stopped: "Not running"
        }
    }
}

extension AppModel {
    func light(for service: String) -> ServiceLight {
        guard let daemon, daemon.running else { return .stopped }
        guard let local = daemon.services?[service] else { return .connecting }
        if local.connected { return .connected }
        // A few quick retries are normal (sleep, network change, a redeploy). Repeated failures mean offline.
        return !local.lastError.isEmpty && local.reconnectAttempt >= 3 ? .offline : .connecting
    }
}

/// A small status light. Color crossfades; "connecting" breathes in opacity only, and holds still with Reduce Motion.
struct StatusDot: View {
    let light: ServiceLight
    var size: CGFloat = 8
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var dim = false

    var body: some View {
        Circle()
            .fill(light.color)
            .frame(width: size, height: size)
            .opacity(light == .connecting && dim && !reduceMotion ? 0.35 : 1)
            .animation(Motion.easeOut, value: light)
            .onChange(of: light, initial: true) { _, new in
                dim = false
                guard new == .connecting, !reduceMotion else { return }
                withAnimation(.easeInOut(duration: 0.9).repeatForever(autoreverses: true)) { dim = true }
            }
            .accessibilityLabel(light.label)
    }
}

// MARK: Buttons

/// Press feedback for any button: a quick, subtle scale. Release is slightly slower than press.
struct PressableStyle: ButtonStyle {
    var prominent = false

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            .background(
                RoundedRectangle(cornerRadius: 7, style: .continuous)
                    .fill(prominent ? AnyShapeStyle(Color.accentColor) : AnyShapeStyle(.quaternary))
            )
            .foregroundStyle(prominent ? AnyShapeStyle(.white) : AnyShapeStyle(.primary))
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(configuration.isPressed ? Motion.press : Motion.release, value: configuration.isPressed)
            .contentShape(Rectangle())
    }
}

/// Plain icon/text button that still answers presses.
struct QuietPressStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .contentShape(Rectangle())
            .opacity(configuration.isPressed ? 0.6 : 1)
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(configuration.isPressed ? Motion.press : Motion.release, value: configuration.isPressed)
    }
}

/// Copies text; the icon swaps to a checkmark with a short blur so it reads as one change, then swaps back.
struct CopyButton: View {
    let text: String
    var label: String = "Copy"
    var compact = false
    @Environment(AppModel.self) private var model
    @State private var copied = false
    @State private var resetTask: Task<Void, Never>?

    var body: some View {
        Button {
            model.copy(text)
            withAnimation(Motion.easeOut) { copied = true }
            resetTask?.cancel()
            resetTask = Task {
                try? await Task.sleep(for: .seconds(1.4))
                guard !Task.isCancelled else { return }
                withAnimation(Motion.easeOut) { copied = false }
            }
        } label: {
            HStack(spacing: 5) {
                Image(systemName: copied ? "checkmark" : "doc.on.doc")
                    .contentTransition(.symbolEffect(.replace))
                    .frame(width: 14)
                if !compact {
                    Text(copied ? "Copied" : label)
                        .contentTransition(.opacity)
                }
            }
            .foregroundStyle(copied ? AnyShapeStyle(.green) : AnyShapeStyle(.primary))
        }
        .buttonStyle(compact ? AnyButtonStyle(QuietPressStyle()) : AnyButtonStyle(PressableStyle()))
        .help("Copy to the clipboard")
        .accessibilityLabel(copied ? "Copied" : label)
    }
}

struct AnyButtonStyle: ButtonStyle {
    private let make: (Configuration) -> AnyView
    init<S: ButtonStyle>(_ style: S) { make = { AnyView(style.makeBody(configuration: $0)) } }
    func makeBody(configuration: Configuration) -> some View { make(configuration) }
}

// MARK: Pairing code

/// The pairing code, big and monospaced. Appears from 0.96 with a fade (never from nothing).
struct PairCodeView: View {
    let pair: PairResult
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var shown = false

    var body: some View {
        VStack(spacing: 8) {
            Text(pair.grouped)
                .font(.system(size: 26, weight: .semibold, design: .monospaced))
                .tracking(2)
                .textSelection(.enabled)
                .accessibilityLabel("Pairing code \(pair.code.map(String.init).joined(separator: " "))")
            HStack(spacing: 10) {
                CopyButton(text: pair.code, label: "Copy code")
                Text("Expires \(pair.expiryDate, style: .relative)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .monospacedDigit()
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity)
        .background(RoundedRectangle(cornerRadius: 10, style: .continuous).fill(.quaternary.opacity(0.6)))
        .scaleEffect(shown || reduceMotion ? 1 : 0.96)
        .opacity(shown ? 1 : 0)
        .onAppear { withAnimation(Motion.reveal) { shown = true } }
    }
}

// MARK: Small pieces

struct ErrorBanner: View {
    let failure: EngineFailure
    var onDismiss: (() -> Void)?

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
            VStack(alignment: .leading, spacing: 2) {
                Text(failure.message).font(.callout).fixedSize(horizontal: false, vertical: true)
                if let fix = failure.fix, !fix.isEmpty {
                    Text(fix).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
                }
            }
            Spacer(minLength: 0)
            if let onDismiss {
                Button(action: onDismiss) { Image(systemName: "xmark").font(.caption) }
                    .buttonStyle(QuietPressStyle())
                    .accessibilityLabel("Dismiss")
            }
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(.orange.opacity(0.1)))
        .transition(.opacity)
    }
}

extension Error {
    var asFailure: EngineFailure {
        (self as? EngineFailure) ?? EngineFailure(code: "error", message: localizedDescription)
    }
}

/// Runs an engine action, tracking busy state and errors for a view.
@MainActor
@Observable
final class ActionState {
    private(set) var busy = false
    var failure: EngineFailure?

    func perform(_ work: @escaping @MainActor () async throws -> Void) {
        guard !busy else { return }
        busy = true
        failure = nil
        Task {
            defer { busy = false }
            do { try await work() } catch { withAnimation(Motion.easeOut) { failure = error.asFailure } }
        }
    }
}
