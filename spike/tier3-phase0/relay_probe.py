#!/usr/bin/env python3
import argparse
import hashlib
import hmac
import json
import os
import socket
import stat
import threading
import time


def token(secret, profile, epoch, expires, nonce):
    payload = f"{profile}|{epoch}|{expires}|{nonce}".encode()
    return hmac.new(secret.encode(), payload, hashlib.sha256).hexdigest()


def run_server(path, secret):
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(path)
    os.chmod(path, 0o600)
    listener.listen(16)
    used_nonces = set()
    committed = set()
    lock = threading.Lock()
    print(f"READY mode={stat.S_IMODE(os.stat(path).st_mode):04o}", flush=True)

    def handle(conn):
        with conn:
            request = json.loads(conn.makefile("rb").readline())
            profile = request.get("profile")
            epoch = request.get("epoch")
            expires = request.get("expires")
            nonce = request.get("nonce")
            supplied = request.get("token", "")
            expected = token(secret, profile, epoch, expires, nonce)
            reason = None
            if profile != "probe-profile":
                reason = "wrong-profile"
            elif epoch != 7:
                reason = "wrong-epoch"
            elif expires < int(time.time()):
                reason = "expired"
            elif not hmac.compare_digest(supplied, expected):
                reason = "bad-token"
            with lock:
                if reason is None and nonce in used_nonces:
                    reason = "replayed"
                if reason is None:
                    used_nonces.add(nonce)
                    mutation = request["mutation_id"]
                    duplicate = mutation in committed
                    committed.add(mutation)
                else:
                    duplicate = None
            reply = {"ok": reason is None, "reason": reason, "duplicate": duplicate}
            conn.sendall(json.dumps(reply).encode() + b"\n")

    while True:
        conn, _ = listener.accept()
        threading.Thread(target=handle, args=(conn,), daemon=True).start()


def exchange(path, request):
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    conn.connect(path)
    conn.sendall(json.dumps(request).encode() + b"\n")
    reply = json.loads(conn.makefile("rb").readline())
    conn.close()
    return reply


def request(secret, nonce, mutation_id="one-mutation", profile="probe-profile", epoch=7, expires=None):
    if expires is None:
        expires = int(time.time()) + 300
    return {
        "profile": profile,
        "epoch": epoch,
        "expires": expires,
        "nonce": nonce,
        "mutation_id": mutation_id,
        "token": token(secret, profile, epoch, expires, nonce),
    }


def run_client(path, secret):
    replies = []
    started = time.monotonic()
    for i in range(100):
        replies.append(exchange(path, request(secret, f"reconnect-{i}")))
    elapsed = time.monotonic() - started
    committed = sum(1 for reply in replies if reply["ok"] and not reply["duplicate"])
    deduplicated = sum(1 for reply in replies if reply["ok"] and reply["duplicate"])

    replay = request(secret, "replay-token", mutation_id="replay-mutation")
    replay_first = exchange(path, replay)
    replay_second = exchange(path, replay)
    expired = exchange(path, request(secret, "expired", expires=int(time.time()) - 1))
    wrong_profile = exchange(path, request(secret, "wrong-profile", profile="other-profile"))
    wrong_epoch = exchange(path, request(secret, "wrong-epoch", epoch=8))
    print(
        json.dumps(
            {
                "connections": len(replies),
                "elapsed_seconds": round(elapsed, 6),
                "connections_per_second": round(len(replies) / elapsed, 1),
                "committed_mutations": committed,
                "deduplicated_mutations": deduplicated,
                "replay_first": replay_first,
                "replay_second": replay_second,
                "expired": expired,
                "wrong_profile": wrong_profile,
                "wrong_epoch": wrong_epoch,
            },
            sort_keys=True,
        )
    )


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("server", "client"))
    parser.add_argument("socket")
    parser.add_argument("secret")
    args = parser.parse_args()
    if args.mode == "server":
        run_server(args.socket, args.secret)
    else:
        run_client(args.socket, args.secret)


if __name__ == "__main__":
    main()
