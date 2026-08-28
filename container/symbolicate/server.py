#!/usr/bin/env python3
"""Symbolication server — receives a dSYM binary + frames, returns symbolicated frames.

Request (from the Worker, see src/api/symbolicate.ts):
  POST /symbolicate
  Content-Type: application/octet-stream
  X-Frames: [{"address": "0x1a2b", "module": "App"}, ...]
  <body: the raw binary, streamed from R2 — never base64>

The body is spooled straight to a temp file, so memory stays flat regardless
of dSYM size.
"""
import json, os, shutil, subprocess, sys, tempfile
from http.server import HTTPServer, BaseHTTPRequestHandler

MAX_FRAMES = 200
SYMBOLIZER_TIMEOUT_S = 5


class SymbolicateHandler(BaseHTTPRequestHandler):
    def _json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        try:
            content_length = int(self.headers.get('Content-Length', '0'))
            frames = json.loads(self.headers.get('X-Frames', '[]'))[:MAX_FRAMES]
        except (ValueError, TypeError):
            self._json(400, {'error': 'bad Content-Length or X-Frames'})
            return
        if content_length <= 0 or not frames:
            self._json(400, {'error': 'missing binary or frames'})
            return

        with tempfile.NamedTemporaryFile(suffix='.dSYM', delete=False) as tmp:
            # copyfileobj streams in 64 KB chunks — no whole-body read.
            remaining = content_length
            while remaining > 0:
                chunk = self.rfile.read(min(65536, remaining))
                if not chunk:
                    break
                tmp.write(chunk)
                remaining -= len(chunk)
            tmp_path = tmp.name

        try:
            if remaining != 0:
                self._json(400, {'error': 'truncated body'})
                return
            results = []
            for frame in frames:
                addr = str(frame.get('address', '0x0'))
                proc = subprocess.run(
                    ['llvm-symbolizer', '--obj=' + tmp_path, addr],
                    capture_output=True, text=True, timeout=SYMBOLIZER_TIMEOUT_S,
                )
                lines = proc.stdout.strip().split('\n')
                symbol = lines[0] if lines else '?'
                fileline = lines[1] if len(lines) > 1 else '?'
                parts = fileline.rsplit(':', 1)
                filename = parts[0] if parts else '?'
                lineno = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else 0
                results.append({'function': symbol, 'filename': filename, 'lineno': lineno})
            self._json(200, {'frames': results})
        except subprocess.TimeoutExpired:
            self._json(504, {'error': 'llvm-symbolizer timed out'})
        finally:
            os.unlink(tmp_path)

    def log_message(self, format, *args):
        sys.stderr.write(f'[symbolicate] {format}\n' % args)


if __name__ == '__main__':
    server = HTTPServer(('0.0.0.0', 8080), SymbolicateHandler)
    print('Symbolicate server on :8080', flush=True)
    server.serve_forever()
