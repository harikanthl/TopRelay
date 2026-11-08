# Verifies X-TopRelay-Caller (docs/protocol.md "Agent → local app"). Python 3.8+, standard library only.
# BEGIN verifier
import hashlib, hmac, time, urllib.parse

def verify_toprelay_caller(key: bytes, header: str, method: str, target: str, body: bytes, now=None):
    """Returns the caller's client id if the header is valid, else None. target is the raw request path + query."""
    parts = [p.strip() for p in (header or "").split(";")]
    f = dict(p.split("=", 1) for p in parts[1:] if "=" in p)
    if parts[0] != "v1" or not all(k in f for k in ("client", "ts", "nonce", "sig")) or not f["ts"].isdigit():
        return None
    if abs((time.time() if now is None else now) - int(f["ts"])) > 60:
        return None
    msg = "\n".join(["toprelay-caller-v1", method, target, f["client"], f["ts"], f["nonce"], hashlib.sha256(body).hexdigest()])
    if not hmac.compare_digest(hmac.new(key, msg.encode(), hashlib.sha256).hexdigest(), f["sig"]):
        return None
    return urllib.parse.unquote(f["client"])
# END verifier
if __name__ == "__main__":  # the protocol.md test vector
    import sys
    key, header, method, target, body, now = sys.argv[1:7]
    print(verify_toprelay_caller(key.encode(), header, method, target, body.encode(), int(now)))
