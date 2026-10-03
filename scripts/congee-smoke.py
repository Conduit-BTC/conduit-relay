#!/usr/bin/env python3
"""Smoke an image using disposable synthetic data; never print runtime logs."""

import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]


def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError("Docker command failed: " + args[0])
    return result.stdout.strip()


def get(port, path, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
    try:
        conn.request("GET", path, headers=headers or {})
        response = conn.getresponse()
        return response.status, dict(response.getheaders()), response.read()
    finally:
        conn.close()


def upgrade(port, origin):
    with socket.create_connection(("127.0.0.1", port), timeout=3) as conn:
        headers = ["GET / HTTP/1.1", "Host: localhost", "Connection: Upgrade",
                   "Upgrade: websocket", "Sec-WebSocket-Version: 13",
                   "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ=="]
        if origin is not None:
            headers.append("Origin: " + origin)
        conn.sendall(("\r\n".join(headers) + "\r\n\r\n").encode("ascii"))
        status_line = bytearray()
        while b"\r\n" not in status_line:
            chunk = conn.recv(1024)
            if not chunk or len(status_line) + len(chunk) > 8192:
                raise RuntimeError("Incomplete WebSocket response")
            status_line.extend(chunk)
        return int(status_line.split(b" ", 2)[1])


def main():
    if len(sys.argv) != 2:
        raise RuntimeError("Usage: congee-smoke.py IMAGE")
    image = sys.argv[1]
    version = (ROOT / "congee/VERSION").read_text().strip()
    if docker("run", "--rm", image, "version") != version:
        raise RuntimeError("Binary version does not match pinned version")
    container = None
    with tempfile.TemporaryDirectory(prefix="congee-smoke-") as directory:
        config_dir = Path(directory) / "config"
        config_dir.mkdir()
        config = json.loads((ROOT / "congee/config.example.json").read_text())
        config["nips"]["enabled"] = [1, 11, 17, 42, 50, 77]
        config["nip77"]["upstream_enabled"] = False
        config["nip42"]["relay_url"] = "wss://relay.conduit.market"
        (config_dir / "config.json").write_text(json.dumps(config))
        try:
            container = docker("run", "--detach", "--user", f"{os.getuid()}:{os.getgid()}",
                               "--publish", "127.0.0.1::3334",
                               "--mount", f"type=bind,src={directory},dst=/data",
                               "--env", "ENABLE_ADMIN_UI=false", image)
            port = int(docker("port", container, "3334/tcp").rsplit(":", 1)[1])
            deadline = time.monotonic() + 45
            while True:
                try:
                    if get(port, "/health")[0] == 200:
                        break
                except (OSError, http.client.HTTPException):
                    pass
                if time.monotonic() >= deadline:
                    raise RuntimeError("Production entrypoint did not become healthy")
                time.sleep(0.25)
            status, _, body = get(port, "/", {"Accept": "application/nostr+json"})
            info = json.loads(body)
            if status != 200 or info.get("version") != version:
                raise RuntimeError("NIP-11 version mismatch")
            if not {1, 11, 17, 42, 50, 77}.issubset(info.get("supported_nips", [])):
                raise RuntimeError("Required NIPs missing")
            for origin in (None, "null", "https://example.com", "http://localhost:7000",
                           "https://shop.conduit.market", "https://sell.conduit.market",
                           "https://fix-search.conduit-market-coo.pages.dev",
                           "https://a1b2c3.conduit-merchant-33n.pages.dev"):
                if upgrade(port, origin) != 101:
                    raise RuntimeError("Public Nostr client rejected")
                discovery_headers = {"Accept": "application/nostr+json"}
                if origin is not None:
                    discovery_headers["Origin"] = origin
                status, headers, _ = get(port, "/", discovery_headers)
                if status != 200 or headers.get("Access-Control-Allow-Origin") != "*":
                    raise RuntimeError("Public discovery CORS missing")
            conn = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
            try:
                conn.request("OPTIONS", "/", headers={
                    "Origin": "https://example.com",
                    "Access-Control-Request-Method": "GET",
                    "Access-Control-Request-Headers": "Accept"})
                response = conn.getresponse()
                if (response.status != 204 or
                        response.getheader("Access-Control-Allow-Origin") != "*" or
                        response.getheader("Access-Control-Allow-Methods") != "GET, OPTIONS" or
                        response.getheader("Access-Control-Allow-Headers") != "Accept"):
                    raise RuntimeError("Public discovery preflight failed")
                response.read()
            finally:
                conn.close()
            docker("exec", container, "test", "-f", "/web/admin/build/index.html")
            print("PASS: pinned binary, health, required NIPs, public WebSockets, CORS, and admin assets")
        finally:
            if container:
                docker("rm", "--force", "--volumes", container)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, http.client.HTTPException) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
