"""A stand-in for a Slack incoming webhook, for end-to-end tests.

Answers every POST with 200 and, if given a file name, appends each body to
it, so you can read exactly what juju would have posted to Slack.

    python3 dashboard/e2e/fake-slack.py [log-file]   # listens on 127.0.0.1:9911
"""
import http.server
import sys

LOG = sys.argv[1] if len(sys.argv) > 1 else None


class Hook(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if LOG:
            with open(LOG, "ab") as f:
                f.write(body + b"\n")
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", 9911), Hook).serve_forever()
