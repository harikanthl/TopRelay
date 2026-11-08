// Drives a real `toprelay up` through TopRelayKit. Used by the agent's end-to-end test:
//   trkit-smoke <config dir> <service>
// Prints one line per step and "ok" at the end; exits 1 on the first failure.
import Foundation
import TopRelayKit

let args = CommandLine.arguments
guard args.count == 3 else {
    FileHandle.standardError.write(Data("usage: trkit-smoke <config dir> <service>\n".utf8))
    exit(2)
}
let relay = TopRelay(configDirectory: URL(fileURLWithPath: args[1]))
let service = args[2]

func check(_ ok: Bool, _ what: String) {
    print(ok ? "pass \(what)" : "FAIL \(what)")
    if !ok { exit(1) }
}

let done = DispatchSemaphore(value: 0)
Task {
    do {
        check(relay.isAgentRunning, "agent running")
        let services = try await relay.services()
        check(services.contains { $0.name == service }, "service listed")
        let update = try await relay.setCredential(service: service, .value(header: "X-App-Token", value: "from-kit"))
        check(update.updated && update.header == "X-App-Token" && update.source == "keyring", "setCredential")
        let ck = try await relay.callerKey(service: service)
        check(ck.key.count == 43 && ck.header == "X-TopRelay-Caller", "callerKey")
        let tools = try await relay.tools(service: service)
        check(tools.service == service, "tools")
        let pair = try await relay.pairingCode(service: service)
        check(pair.code.count == 12 && pair.expiresAt > Date(), "pairingCode")
        let key = try await relay.mintKey(service: service, client: "curl", validFor: "1h")
        check(key.key.hasPrefix("trk1.") && key.snippets["curl"] != nil, "mintKey")
        do {
            _ = try await relay.pairingCode(service: "nosuchservice")
            check(false, "error mapping")
        } catch let e as TopRelayError {
            check(e.code == "no_service" && e.fix != nil, "error mapping")
        }
        let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("trkit-\(UUID().uuidString).txt")
        try Data("hello".utf8).write(to: tmp)
        defer { try? FileManager.default.removeItem(at: tmp) }
        let offer = try await relay.offerFile(service: service, path: tmp.path, contentType: "text/plain", validFor: 60)
        check(offer.uri == "toprelay-file://\(offer.id)" && offer.id.count == 32, "offerFile")
        var seen = 0
        for try await state in relay.stateUpdates() {
            check(state.version == 1 && state.running && state.services[service]?.connected == true, "stateUpdates")
            seen += 1
            if seen == 1 { break }
        }
        let st = try await relay.state()
        check(st.running, "state")
        print("ok")
        exit(0)
    } catch {
        print("FAIL \(error)")
        exit(1)
    }
}
done.wait()
