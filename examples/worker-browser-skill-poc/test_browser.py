#!/usr/bin/env python3
"""Run deterministic CLI/browser checks in the disposable POC container."""
import json
import os
from pathlib import Path
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parent
OUT = Path("/tmp/browser-poc-results")
OUT.mkdir(exist_ok=True)
PAGE = """<!doctype html><html lang="zh-CN"><meta charset="utf-8">
<title>Worker 浏览器技能 POC</title>
<style>body{font:22px sans-serif;margin:40px;max-width:880px}input,button,a{font:inherit;margin:12px;padding:8px}#spacer{height:1200px}</style>
<h1>Worker 浏览器技能 POC</h1><p>安装到选中的 Worker 后，读取页面、点击和输入。</p>
<label>姓名<input id="name"></label><button id="submit">提交</button>
<p id="result" role="status">等待提交</p>
<label>上传文件<input id="upload" type="file"></label><p id="upload-result"></p>
<a href="/download" download="result.txt">下载结果</a>
<div id="spacer"></div><button id="bottom">页面底部</button>
<script>
document.querySelector('#submit').onclick=()=>{document.querySelector('#result').textContent='已提交：'+document.querySelector('#name').value};
document.querySelector('#upload').onchange=e=>{document.querySelector('#upload-result').textContent=e.target.files[0].name};
document.querySelector('#bottom').onclick=()=>{document.querySelector('#bottom').textContent='底部点击成功'};
</script></html>"""

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/download":
            data = "browser-skill-download-ok\n".encode()
            self.send_response(200)
            self.send_header("Content-Disposition", 'attachment; filename="result.txt"')
            self.send_header("Content-Type", "text/plain")
        else:
            data = PAGE.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass

steps = []
def browser(*args, session="task-a", expect=True):
    start = time.monotonic()
    proc = subprocess.run(
        ["bash", str(ROOT / "browser-automation/scripts/browser.sh"), "--json", *args],
        env={**os.environ, "BROWSER_SKILL_SESSION": session},
        capture_output=True, text=True, timeout=45,
    )
    record = {"command": list(args), "session": session, "seconds": round(time.monotonic()-start, 3),
              "code": proc.returncode, "stdout": proc.stdout, "stderr": proc.stderr}
    steps.append(record)
    print(json.dumps(record, ensure_ascii=False), flush=True)
    if expect:
        assert proc.returncode == 0, record
        assert json.loads(proc.stdout)["success"] is True, record
    return proc.stdout

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
url = f"http://127.0.0.1:{server.server_port}"
started = time.monotonic()
try:
    browser("open", url)
    snapshot = json.loads(browser("snapshot", "-i"))["data"]
    name_ref = next(key for key, value in snapshot["refs"].items()
                    if value["role"] == "textbox" and value["name"] == "姓名")
    browser("fill", "@"+name_ref, "可选浏览器技能")
    browser("click", "#submit")
    result = browser("get", "text", "#result")
    assert "已提交：可选浏览器技能" in result, result
    (OUT / "upload.txt").write_text("upload-ok")
    browser("upload", "#upload", str(OUT / "upload.txt"))
    assert "upload.txt" in browser("get", "text", "#upload-result")
    browser("download", 'a[download]', str(OUT / "download.txt"))
    assert (OUT / "download.txt").read_text() == "browser-skill-download-ok\n"
    browser("screenshot", str(OUT / "chinese-form.png"))
    assert (OUT / "chinese-form.png").stat().st_size > 1000
    browser("scroll", "down", "1400")
    browser("click", "#bottom")
    assert "底部点击成功" in browser("get", "text", "#bottom")
    browser("eval", "document.cookie='poc=session-a;path=/'")
    browser("open", url, session="task-b")
    assert "session-a" not in browser("eval", "document.cookie", session="task-b")
    assert "session-a" in browser("eval", "document.cookie")
    summary = {"passed": True, "steps": len(steps), "seconds": round(time.monotonic()-started, 3),
               "scope": "CLI in disposable ARM64 QwenPaw Worker container; no live Agent/Matrix task"}
finally:
    for session in ("task-a", "task-b"):
        browser("close", session=session, expect=False)
    server.shutdown()
    (OUT / "steps.json").write_text(json.dumps(steps, ensure_ascii=False, indent=2))
(OUT / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2))
print(json.dumps(summary, ensure_ascii=False))
