import Darwin
import XCTest
@testable import TopRelayKit

/// A one-connection-at-a-time fake agent on a Unix socket: answers each request with the next canned response
/// and records the requests.
final class FakeAgent: @unchecked Sendable {
    let dir: URL
    var path: String { dir.appendingPathComponent("agent.sock").path }
    private var fd: Int32 = -1
    private let lock = NSLock()
    private var _requests: [String] = []
    var requests: [String] { lock.lock(); defer { lock.unlock() }; return _requests }
    var respond: (String) -> String = { _ in "HTTP/1.0 200 OK\r\n\r\n{}" }

    init() throws {
        dir = URL(fileURLWithPath: "/tmp").appendingPathComponent("trkit-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            let b = Array(path.utf8)
            raw.copyBytes(from: b)
            raw[b.count] = 0
        }
        let r = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) } }
        precondition(r == 0 && listen(fd, 8) == 0)
        let lfd = fd
        Thread {
            while true {
                let c = accept(lfd, nil, nil)
                if c < 0 { return }
                var one: Int32 = 1
                setsockopt(c, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
                var buf = Data()
                var chunk = [UInt8](repeating: 0, count: 4096)
                while true {
                    let n = read(c, &chunk, chunk.count)
                    if n <= 0 { break }
                    buf.append(contentsOf: chunk[0..<n])
                    if let r = buf.range(of: Data("\r\n\r\n".utf8)) {
                        let head = String(decoding: buf[..<r.lowerBound], as: UTF8.self)
                        let len = head.split(separator: "\r\n").first { $0.lowercased().hasPrefix("content-length:") }
                            .flatMap { Int($0.split(separator: ":")[1].trimmingCharacters(in: .whitespaces)) } ?? 0
                        if buf.count - r.upperBound >= len { break }
                    }
                }
                if buf.isEmpty { close(c); continue } // a connect-only probe (isAgentRunning)
                let req = String(decoding: buf, as: UTF8.self)
                self.lock.lock(); self._requests.append(req); self.lock.unlock()
                let out = Data(self.respond(req).utf8)
                out.withUnsafeBytes { _ = write(c, $0.baseAddress, out.count) }
                close(c)
            }
        }.start()
    }

    deinit {
        close(fd)
        try? FileManager.default.removeItem(at: dir)
    }
}

final class TopRelayKitTests: XCTestCase {
    func testRunSendsArgsAndStdinAndDecodes() async throws {
        let agent = try FakeAgent()
        agent.respond = { _ in "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n{\"service\":\"notes\",\"header\":\"X-T\",\"updated\":true,\"source\":\"keyring\"}\n" }
        let relay = TopRelay(configDirectory: agent.dir)
        XCTAssertTrue(relay.isAgentRunning)
        let u = try await relay.setCredential(service: "notes", .value(header: "X-T", value: "s3cret"))
        XCTAssertEqual(u.header, "X-T")
        let req = agent.requests.last!
        XCTAssertTrue(req.hasPrefix("POST /v1/cli HTTP/1.0\r\n"))
        let body = try JSONSerialization.jsonObject(with: Data(req.components(separatedBy: "\r\n\r\n")[1].utf8)) as! [String: Any]
        XCTAssertEqual(body["args"] as? [String], ["set-header", "notes", "--stdin"])
        XCTAssertEqual(body["stdin"] as? String, "X-T: s3cret\n")
        // The secret travels in the body, never in the arguments.
        XCTAssertFalse((body["args"] as! [String]).joined().contains("s3cret"))
    }

    func testErrorsCarryCodeAndFix() async throws {
        let agent = try FakeAgent()
        agent.respond = { _ in "HTTP/1.0 422 Unprocessable Entity\r\n\r\n{\"error\":{\"code\":\"no_service\",\"message\":\"no service named \\\"x\\\"\",\"fix\":\"run `toprelay list`\"}}\n" }
        do {
            _ = try await TopRelay(configDirectory: agent.dir).pairingCode(service: "x")
            XCTFail("no error")
        } catch let e as TopRelayError {
            XCTAssertEqual(e, TopRelayError(code: "no_service", message: "no service named \"x\"", fix: "run `toprelay list`"))
        }
    }

    func testAgentNotRunning() async {
        let relay = TopRelay(configDirectory: URL(fileURLWithPath: "/tmp/trkit-none-\(UUID().uuidString.prefix(6))"))
        XCTAssertFalse(relay.isAgentRunning)
        do {
            _ = try await relay.services()
            XCTFail("no error")
        } catch let e as TopRelayError {
            XCTAssertEqual(e.code, "agent_not_running")
            XCTAssertNotNil(e.fix)
        } catch {
            XCTFail("\(error)")
        }
    }

    func testStateUpdatesStream() async throws {
        let agent = try FakeAgent()
        let s1 = #"{"version":1,"pid":7,"startedAt":"2026-11-07T10:00:00Z","updatedAt":"2026-11-07T10:00:00.123456Z","running":true,"services":{"notes":{"connected":false,"since":null,"lastError":"dial","reconnectAttempt":1,"nextRetryMs":500,"requests":0,"refused":0}}}"#
        let s2 = #"{"version":1,"pid":7,"startedAt":"2026-11-07T10:00:00Z","updatedAt":"2026-11-07T10:00:01Z","running":true,"services":{"notes":{"connected":true,"since":"2026-11-07T10:00:01Z","lastError":"","reconnectAttempt":0,"nextRetryMs":0,"requests":3,"refused":1}}}"#
        agent.respond = { _ in "HTTP/1.0 200 OK\r\nContent-Type: application/x-ndjson\r\n\r\n\(s1)\n\n\(s2)\n" }
        var got: [AgentState] = []
        for try await s in TopRelay(configDirectory: agent.dir).stateUpdates() { got.append(s) }
        XCTAssertEqual(got.count, 2)
        XCTAssertEqual(got[0].services["notes"]?.lastError, "dial")
        XCTAssertEqual(got[1].services["notes"]?.connected, true)
        XCTAssertEqual(got[1].services["notes"]?.requests, 3)
        XCTAssertTrue(agent.requests[0].hasPrefix("GET /v1/state/watch HTTP/1.0"))
    }

    func testRegisterBuildsCLIArguments() async throws {
        let agent = try FakeAgent()
        agent.respond = { _ in "HTTP/1.0 200 OK\r\n\r\n{\"url\":\"https://toprelay.x.workers.dev/notes/mcp\",\"tools\":[],\"warnings\":[],\"firstDeploy\":false,\"subdomain\":\"x\"}" }
        let r = try await TopRelay(configDirectory: agent.dir).register(service: "notes", mcpURL: "unix:///tmp/n.sock",
            credential: .file(header: "X-T", path: "/tmp/tok"), toolsDefault: .deny, pinTeamID: "ABCDE12345")
        XCTAssertEqual(r.url, "https://toprelay.x.workers.dev/notes/mcp")
        let body = try JSONSerialization.jsonObject(with: Data(agent.requests[0].components(separatedBy: "\r\n\r\n")[1].utf8)) as! [String: Any]
        XCTAssertEqual(body["args"] as? [String], ["add", "notes", "--mcp-url", "unix:///tmp/n.sock", "--tools-default", "deny",
                                                    "--header-file", "X-T=/tmp/tok", "--pin-team-id", "ABCDE12345"])
    }
}
