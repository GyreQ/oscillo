#!/usr/bin/env python3
"""假 Anthropic 上游：回一段含 tool_use 的 SSE 流，用于 ai-trace 手动验收。"""
import json
from http.server import HTTPServer, BaseHTTPRequestHandler

SSE = "\n".join([
    'event: message_start',
    'data: ' + json.dumps({"type": "message_start", "message": {"usage": {"input_tokens": 120, "cache_read_input_tokens": 60}}}),
    '',
    'event: content_block_start',
    'data: ' + json.dumps({"type": "content_block_start", "index": 0, "content_block": {"type": "text"}}),
    '',
    'event: content_block_delta',
    'data: ' + json.dumps({"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "我来看一下目录内容"}}),
    '',
    'event: content_block_start',
    'data: ' + json.dumps({"type": "content_block_start", "index": 1, "content_block": {"type": "tool_use", "id": "toolu_manual_1", "name": "Bash"}}),
    '',
    'event: content_block_delta',
    'data: ' + json.dumps({"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": "{\"cmd\": "}}),
    '',
    'event: content_block_delta',
    'data: ' + json.dumps({"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": "\"ls -la\"}"}}),
    '',
    'event: message_delta',
    'data: ' + json.dumps({"type": "message_delta", "delta": {"stop_reason": "tool_use"}, "usage": {"output_tokens": 55}}),
    '',
    'event: message_stop',
    'data: ' + json.dumps({"type": "message_stop"}),
    '',
    '',
])

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        print(f"[fake-upstream] {self.path} 收到 {n} 字节, UA={self.headers.get('User-Agent')}")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        # 分多块写，模拟真实流式
        for i in range(0, len(SSE), 64):
            self.wfile.write(SSE[i:i+64].encode())
            self.wfile.flush()

    def log_message(self, *a):
        pass

print("假上游监听 :9999")
HTTPServer(("127.0.0.1", 9999), H).serve_forever()
