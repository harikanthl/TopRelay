// Verifies X-TopRelay-Caller (docs/protocol.md "Agent → local app"). macOS 10.15+ / iOS 13+, CryptoKit only.
// BEGIN verifier
import CryptoKit
import Foundation

/// Returns the caller's client id if the header is valid, else nil. `target` is the raw request path + query.
func verifyTopRelayCaller(key: Data, header: String, method: String, target: String, body: Data, now: Date = Date()) -> String? {
    let parts = header.split(separator: ";").map { $0.trimmingCharacters(in: .whitespaces) }
    var f: [String: String] = [:]
    for p in parts.dropFirst() { if let eq = p.firstIndex(of: "=") { f[String(p[..<eq])] = String(p[p.index(after: eq)...]) } }
    guard parts.first == "v1", let c = f["client"], let n = f["nonce"], let sig = f["sig"], let tsS = f["ts"], !tsS.isEmpty,
          tsS.allSatisfy({ $0.isASCII && $0.isNumber }), let ts = Double(tsS), abs(now.timeIntervalSince1970 - ts) <= 60,
          sig.count == 64, let sigBytes = Data(hexString: sig) else { return nil }
    let bodyHash = SHA256.hash(data: body).map { String(format: "%02x", $0) }.joined()
    let msg = ["toprelay-caller-v1", method, target, c, tsS, n, bodyHash].joined(separator: "\n")
    guard HMAC<SHA256>.isValidAuthenticationCode(sigBytes, authenticating: Data(msg.utf8), using: SymmetricKey(data: key)) else { return nil }
    return c.removingPercentEncoding
}

extension Data {
    init?(hexString s: String) {
        var out = Data(capacity: s.count / 2), i = s.startIndex
        while i < s.endIndex {
            let j = s.index(i, offsetBy: 2, limitedBy: s.endIndex) ?? s.endIndex
            guard let b = UInt8(s[i..<j], radix: 16), j > s.index(after: i) else { return nil }
            out.append(b); i = j
        }
        self = out
    }
}
// END verifier
// The protocol.md test vector (only when compiled as a script with `swiftc Verify.swift`).
#if VERIFY_MAIN
let a = CommandLine.arguments
print(verifyTopRelayCaller(key: Data(a[1].utf8), header: a[2], method: a[3], target: a[4], body: Data(a[5].utf8),
                           now: Date(timeIntervalSince1970: Double(a[6])!)) ?? "None")
#endif
