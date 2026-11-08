import Foundation
#if canImport(Darwin)
import Darwin
#else
import Glibc
#endif

/// A minimal HTTP/1.0 client over a Unix socket: one request per connection, the response read until the
/// agent closes it. HTTP/1.0 means no chunked encoding, so streams are plain bytes.
enum SocketHTTP {
    struct Response {
        let status: Int
        let body: Data
    }

    /// Opens a connection to the socket, or throws `agent_not_running`.
    static func connect(_ path: String) throws -> Int32 {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw TopRelayError.io("socket() failed: \(String(cString: strerror(errno)))") }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        let capacity = MemoryLayout.size(ofValue: addr.sun_path)
        guard bytes.count < capacity else {
            close(fd)
            throw TopRelayError.io("socket path is longer than \(capacity - 1) bytes: \(path)")
        }
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            raw.copyBytes(from: bytes)
            raw[bytes.count] = 0
        }
        var noSigPipe: Int32 = 1
        #if canImport(Darwin)
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        #endif
        let ok = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Foundation.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard ok == 0 else {
            close(fd)
            throw TopRelayError.agentNotRunning(path)
        }
        return fd
    }

    static func writeAll(_ fd: Int32, _ data: Data) throws {
        try data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) in
            var off = 0
            while off < raw.count {
                let n = write(fd, raw.baseAddress! + off, raw.count - off)
                if n < 0 {
                    if errno == EINTR { continue }
                    throw TopRelayError.io("write: \(String(cString: strerror(errno)))")
                }
                off += n
            }
        }
    }

    static func request(method: String, path: String, body: Data?) -> Data {
        var head = "\(method) \(path) HTTP/1.0\r\nHost: agent\r\n"
        if let body {
            head += "Content-Type: application/json\r\nContent-Length: \(body.count)\r\n"
        }
        head += "\r\n"
        var d = Data(head.utf8)
        if let body { d.append(body) }
        return d
    }

    /// Splits "HTTP/1.x NNN …\r\nheaders\r\n\r\nbody".
    static func parseHead(_ buf: Data) -> (status: Int, bodyStart: Int)? {
        guard let range = buf.range(of: Data("\r\n\r\n".utf8)) else { return nil }
        let head = String(decoding: buf[buf.startIndex..<range.lowerBound], as: UTF8.self)
        let parts = head.split(separator: " ", maxSplits: 2)
        guard parts.count >= 2, parts[0].hasPrefix("HTTP/"), let status = Int(parts[1]) else { return (0, range.upperBound) }
        return (status, range.upperBound)
    }

    /// One request, the whole response.
    static func roundTrip(socket path: String, method: String, path urlPath: String, body: Data?) throws -> Response {
        let fd = try connect(path)
        defer { close(fd) }
        try writeAll(fd, request(method: method, path: urlPath, body: body))
        var buf = Data()
        var chunk = [UInt8](repeating: 0, count: 64 << 10)
        while true {
            let n = read(fd, &chunk, chunk.count)
            if n < 0 {
                if errno == EINTR { continue }
                throw TopRelayError.io("read: \(String(cString: strerror(errno)))")
            }
            if n == 0 { break }
            buf.append(contentsOf: chunk[0..<n])
            if buf.count > 32 << 20 { throw TopRelayError.io("response over 32 MiB") }
        }
        guard let (status, start) = parseHead(buf), status > 0 else { throw TopRelayError.io("bad response from the agent") }
        return Response(status: status, body: buf.subdata(in: start..<buf.endIndex))
    }

    /// A streamed GET: calls `line` for every non-empty line of the body until the agent closes the stream, or
    /// `cancel` closes the socket.
    static func stream(socket path: String, path urlPath: String, fdReady: (Int32) -> Void, fdDone: (Int32) -> Void,
                       line: (Data) -> Void) throws {
        let fd = try connect(path)
        // fdDone runs before close, so nobody can shut down this number after the system reuses it.
        defer { fdDone(fd); close(fd) }
        fdReady(fd)
        try writeAll(fd, request(method: "GET", path: urlPath, body: nil))
        var buf = Data()
        var inBody = false
        var chunk = [UInt8](repeating: 0, count: 16 << 10)
        while true {
            let n = read(fd, &chunk, chunk.count)
            if n < 0 {
                if errno == EINTR { continue }
                return // closed by cancel
            }
            if n == 0 { return }
            buf.append(contentsOf: chunk[0..<n])
            if !inBody {
                guard let (status, start) = parseHead(buf) else { continue }
                guard status == 200 else { throw TopRelayError.io("agent answered HTTP \(status)") }
                buf = buf.subdata(in: start..<buf.endIndex)
                inBody = true
            }
            while let nl = buf.firstIndex(of: 0x0A) {
                let l = buf.subdata(in: buf.startIndex..<nl)
                buf = buf.subdata(in: buf.index(after: nl)..<buf.endIndex)
                if !l.allSatisfy({ $0 == 0x20 || $0 == 0x0D }) { line(l) }
            }
        }
    }
}
