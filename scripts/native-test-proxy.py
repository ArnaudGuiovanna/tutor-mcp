#!/usr/bin/env python3
"""Loopback-only TLS terminator for the disposable institution acceptance."""
import argparse
import http.client
import http.server
import ssl

parser = argparse.ArgumentParser()
parser.add_argument("--port", type=int, required=True)
parser.add_argument("--upstream", type=int, required=True)
parser.add_argument("--certificate", required=True)
parser.add_argument("--key", required=True)
args = parser.parse_args()


class Proxy(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.forward()

    def do_POST(self):
        self.forward()

    def forward(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length < 0 or length > 2 * 1024 * 1024:
            self.send_error(413)
            return
        upstream = http.client.HTTPConnection("127.0.0.1", args.upstream, timeout=60)
        try:
            headers = {key: value for key, value in self.headers.items()
                       if key.lower() not in {"connection", "transfer-encoding"}}
            headers["X-Forwarded-Proto"] = "https"
            upstream.request(self.command, self.path, self.rfile.read(length), headers)
            response = upstream.getresponse()
            body = response.read()
            self.send_response(response.status)
            for key, value in response.getheaders():
                if key.lower() not in {"connection", "content-length", "transfer-encoding"}:
                    self.send_header(key, value)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        finally:
            upstream.close()

    def log_message(self, *_):
        # Authentication headers and signed request bodies are never logged.
        pass


server = http.server.ThreadingHTTPServer(("127.0.0.1", args.port), Proxy)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(args.certificate, args.key)
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
