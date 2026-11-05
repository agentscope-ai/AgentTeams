"""Demo-only allowlisted model proxy; no access to other AT API paths."""

from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os


class Proxy(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        self.forward()

    def do_POST(self):
        self.forward()

    def forward(self):
        allowed = {("GET", "/v1/models"), ("POST", "/v1/chat/completions"), ("POST", "/v1/responses")}
        if (self.command, self.path) not in allowed or self.headers.get("Authorization") != "Bearer at-isolated-model-proxy":
            self.send_error(403); return
        size = int(self.headers.get("Content-Length", "0"))
        if size > 4 * 1024 * 1024:
            self.send_error(413); return
        body = self.rfile.read(size) if size else None
        if body and json.loads(body).get("model") != os.environ["AUTHORIZED_MODEL"]:
            self.send_error(403); return
        connection = HTTPConnection("host.docker.internal", 18080, timeout=300)
        try:
            connection.request(self.command, self.path, body=body, headers={
                "Host": "aigw-local.agentteams.io", "Content-Type": "application/json",
                "Authorization": "Bearer " + os.environ["AUTHORIZED_MODEL_KEY"],
            })
            response = connection.getresponse()
            self.send_response(response.status)
            for key in ("Content-Type", "Content-Length", "x-request-id"):
                if response.getheader(key):
                    self.send_header(key, response.getheader(key))
            self.send_header("Connection", "close"); self.end_headers()
            while chunk := response.read1(8192):
                self.wfile.write(chunk); self.wfile.flush()
            self.close_connection = True
        except (OSError, TimeoutError):
            self.close_connection = True
        finally:
            connection.close()


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Proxy).serve_forever()
