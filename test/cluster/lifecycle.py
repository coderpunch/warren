# Lifecycle checks for a warren deployment on the kind cluster of setup.sh.
# Start this script with run.sh. The file README.md gives the procedure.
import asyncio, json, subprocess, sys, threading, time, urllib.request, urllib.error
import websockets

NODE = subprocess.run(["docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "warren-dev-control-plane"], capture_output=True, text=True).stdout.strip()
W = f"http://{NODE}:30080"
ADMIN = f"http://{NODE}:30090"
KEY = "connection-key-0123456789abcdef0123456789"
fails = []
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def check(name, cond, extra=""):
    print(("PASS " if cond else "FAIL ") + name + (" " + str(extra)[:200] if (not cond or extra) else ""), flush=True)
    if not cond: fails.append(name)

def call(method, path, user="alice", body=None, template="personal", chat=None, timeout=70, key=KEY, base=W):
    h = {}
    if key: h["Authorization"] = "Bearer " + key
    if user:
        h["X-User-Id"] = user; h["X-Terminal-Template-Id"] = template; h["X-Session-Id"] = chat or "chat-1"
        if chat: h["X-Terminal-Chat-Id"] = chat
    if body is not None: h["Content-Type"] = "application/json"; body = json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=body, method=method, headers=h)
    try:
        r = opener.open(req, timeout=timeout)
        return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()

def kubectl(*args):
    return subprocess.run(["docker", "exec", "warren-dev-control-plane", "kubectl", *args], capture_output=True, text=True).stdout

def pods():
    return json.loads(kubectl("-n", "warren-sandboxes", "get", "pods", "-o", "json"))["items"]

def run(cmd, user="alice", **kw):
    s, _, b = call("POST", "/execute?wait=20", user=user, body={"command": cmd}, **kw)
    return s, (json.dumps(json.loads(b)) if s == 200 else b.decode())

s, _, b = call("GET", "/api/v1/terminals/templates", user=None)
check("templates list", s == 200 and len(json.loads(b)) == 5, b)

# ---- cold start ----
check("no sandbox at the start", len(pods()) == 0, len(pods()))
t0 = time.time()
s, _, b = call("GET", "/openapi.json")
dt = time.time() - t0
check("cold start: tool list comes immediately and is complete", s == 200 and dt < 5 and len(json.loads(b)["paths"]) >= 15, (s, round(dt, 1)))
s, _, b = call("GET", "/files/cwd", timeout=10)
check("cold start: short-deadline call gives 200 or 503 with Retry-After", s in (200, 503), s)
t1 = time.time()
s, out = run("echo cold-$((6*7))")
cold = time.time() - t0
check("cold start: first command works", s == 200 and "cold-42" in out, out)
print(f"INFO cold start to first command output: {cold:.1f} s", flush=True)

p = pods()
check("one pod for alice", len(p) == 1, len(p))
pod = p[0]
labels, ann = pod["metadata"]["labels"], pod["metadata"]["annotations"]
check("pod labels", labels.get("app.kubernetes.io/managed-by") == "warren" and labels.get("warren.coderpunch.io/template") == "personal" and labels.get("warren.coderpunch.io/scope") == "user", labels)
check("pod annotations", ann.get("warren.coderpunch.io/context-id") == "user:alice" and ann.get("warren.coderpunch.io/created-by") == "alice", ann)
check("no service account token in the pod", pod["spec"].get("automountServiceAccountToken") is False)
secname = [e for e in pod["spec"]["containers"][0]["env"] if e["name"] == "OPEN_TERMINAL_API_KEY"][0]["valueFrom"]["secretKeyRef"]["name"]
check("the Secret of a start has its own name", secname != pod["metadata"]["name"] and secname.startswith(pod["metadata"]["name"] + "-"), secname)
sec = json.loads(kubectl("-n", "warren-sandboxes", "get", "secret", secname, "-o", "json"))
check("Secret is owned by the pod", sec["metadata"].get("ownerReferences", [{}])[0].get("uid") == pod["metadata"]["uid"])
# The policy "personal" sets no limits, thus the pod has the limits of warren: 2 processors and 2Gi.
LIMITS = ["OPEN_TERMINAL_CPU_LIMIT", "OPEN_TERMINAL_CPU_COUNT", "OPEN_TERMINAL_MEMORY_LIMIT", "OPEN_TERMINAL_MEMORY_BYTES"]
names = [e["name"] for e in pod["spec"]["containers"][0]["env"]]
limenv = {e["name"]: e.get("value") for e in pod["spec"]["containers"][0]["env"] if e["name"] in LIMITS}
check("limit variables in the pod", limenv == dict(zip(LIMITS, ["2", "2", "2Gi", "2147483648"])), limenv)
at = names.index("OPEN_TERMINAL_MULTI_USER") + 1 if "OPEN_TERMINAL_MULTI_USER" in names else -1
check("limit variables come after the variables of warren and before the variables of the policy", at > 0 and names[at:at + 4] == LIMITS and "SHARED_SETTING" in names[at + 4:], names)
reslim = pod["spec"]["containers"][0]["resources"]["limits"]
check("limit variables agree with the limits of the container", reslim.get("cpu") == limenv.get("OPEN_TERMINAL_CPU_LIMIT") and reslim.get("memory") == limenv.get("OPEN_TERMINAL_MEMORY_LIMIT"), reslim)

s, out = run("echo found=$(env | grep -c 0123456789abcdef); ls /var/run/secrets/kubernetes.io 2>&1 | head -1")
text = "".join(o["data"] for o in json.loads(out)["output"]) if s == 200 else out
check("sandbox has no connection key and no service account token", s == 200 and "found=0" in text and "No such file" in text, text)
# Open Terminal removes the variables that start with OPEN_TERMINAL_ from the commands that it starts.
s, out = run("echo limits=$(env | grep -c -E '^OPEN_TERMINAL_(CPU|MEMORY)_')")
text = "".join(o["data"] for o in json.loads(out)["output"]) if s == 200 else out
check("a command in the sandbox does not get the limit variables", s == 200 and "limits=0" in text, text)

# ---- warm path ----
t0 = time.time(); s, _, b = call("GET", "/files/cwd"); warm = time.time() - t0
cwd = json.loads(b).get("cwd") if s == 200 else None
check("warm request", s == 200 and cwd == "/home/user" and warm < 1, (s, b[:80], round(warm, 2)))
s, _, b = call("GET", "/openapi.json")
check("warm tool list is filtered by the sandbox", s == 200 and "/execute" in json.loads(b)["paths"], s)
s, _, b = call("POST", "/files/write", body={"path": "/home/user/secret-of-alice.txt", "content": "alice-private-data\n"})
check("alice writes a file", s == 200, b)
# Open WebUI reads the system information again each 5 seconds while a person has it open.
s, _, b = call("GET", "/system/info")
info = json.loads(b) if s == 200 else {}
check("system information comes from a running sandbox", s == 200 and "cpu" in info and "memory" in info and "hostname" in info, (s, b[:120]))

# ---- isolation between users ----
s, out = run("cat /home/user/secret-of-alice.txt 2>&1; ls /home/user", user="bob")
check("bob has a different sandbox and cannot read the file of alice", s == 200 and "alice-private-data" not in out, out)
check("two pods", len(pods()) == 2, len(pods()))
s, out = run("cat /home/user/secret-of-alice.txt")
check("alice reads her file", "alice-private-data" in out, out)
ip_alice = [x for x in pods() if x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] == "user:alice"][0]["status"]["podIP"]
pykey = "import urllib.request,sys\ntry:\n  r=urllib.request.urlopen('http://%s:8000/files/cwd',timeout=5); print('status',r.status)\nexcept Exception as e: print('result',getattr(e,'code',e))" % ip_alice
s, out = run("python3 -c \"%s\"" % pykey.replace('"', '\\"'), user="bob")
check("bob cannot connect to the sandbox of alice", "timed out" in out, out)

# ---- WebSocket terminal through the cluster ----
async def ws():
    hdr = {"X-User-Id": "alice", "X-Terminal-Template-Id": "personal", "X-Session-Id": "chat-1"}
    s, _, b = call("POST", "/api/terminals")
    sid = json.loads(b)["id"]
    async with websockets.connect(f"ws://{NODE}:30080/api/terminals/{sid}?user_id=alice", additional_headers=hdr, proxy=None) as c:
        await c.send(json.dumps({"type": "auth", "token": KEY, "chat_id": "chat-1"}))
        await c.send(b"echo ws-$((40+2))\n")
        buf = b""
        try:
            while b"ws-42" not in buf:
                m = await asyncio.wait_for(c.recv(), 10); buf += m if isinstance(m, bytes) else m.encode()
        except Exception as e: pass
        check("interactive terminal over WebSocket", b"ws-42" in buf, buf[-120:])
        return c, sid
asyncio.run(ws())

# ---- instance API ----
s, _, b = call("GET", "/api/v1/terminals/templates/personal/instance?user_id=alice", user=None)
check("instance status running", s == 200 and json.loads(b)["status"] == "running", b)
s, _, b = call("GET", "/api/v1/terminals/templates/personal/instance?user_id=nobody", user=None)
check("instance status stopped for a user with no sandbox", s == 200 and json.loads(b)["status"] == "stopped", b)
s, _, b = call("GET", "/api/v1/terminals/templates/personal/instances", user=None)
inst = json.loads(b)
check("instance list", s == 200 and sorted(i["context_id"] for i in inst) == ["user:alice", "user:bob"] and all(i["status"] == "running" for i in inst), b)

# ---- limit for each user (max 2) ----
s1, o1 = run("echo a", chat="chat-a", template="by-chat")
s2, _, b2 = call("POST", "/execute?wait=5", body={"command": "echo b"}, chat="chat-b", template="by-chat")
check("second sandbox of alice starts", s1 == 200, o1)
check("third sandbox of alice is refused with 429", s2 == 429, (s2, b2))

# ---- restart by a manager keeps the owner ----
s, _, b = call("POST", "/api/v1/terminals/templates/by-chat/restart", user=None, body={"user_id": "manager", "chat_id": "chat-a", "model_id": None}, timeout=150)
check("restart by a manager waits and reports running", s == 200 and json.loads(b).get("status") == "running", b)
owner = [x["metadata"]["annotations"]["warren.coderpunch.io/created-by"] for x in pods() if x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] == "chat:chat-a" and "deletionTimestamp" not in x["metadata"]]
check("after the restart, the sandbox stays on the account of alice", owner == ["alice"], owner)

# ---- restart of warren: it finds its sandboxes ----
uid_before = {x["metadata"]["name"]: x["metadata"]["uid"] for x in pods()}
kubectl("-n", "warren-system", "rollout", "restart", "deploy/warren")
kubectl("-n", "warren-system", "rollout", "status", "deploy/warren", "--timeout=90s")
time.sleep(3)
ok = False
for _ in range(20):
    try:
        s, out = run("cat /home/user/secret-of-alice.txt")
        if s == 200: ok = True; break
    except Exception as e: pass
    time.sleep(2)
check("after a restart of warren, the sandbox of alice works with the same pod", ok and "alice-private-data" in out and {x["metadata"]["name"]: x["metadata"]["uid"] for x in pods()} == uid_before, out)

# ---- an open terminal WebSocket with no use does not keep a sandbox ----
# An open terminal WebSocket is not a use of its sandbox: only the messages of its client are, and not the keepalive message.
# The WebSocket of an agent session is the exception: it keeps its sandbox while it is open (docs/policies.md, section 10.4).
# This terminal WebSocket of alice stays open in a second thread, until the idle check at the end of this script.
# Its client sends only the message that the terminal of Open WebUI sends on a timer. Warren must stop the sandbox
# after its idle time, and it must close the WebSocket with the code 4004. The checks below do not use this sandbox.
idle_ws = {"open": False, "pings": 0, "code": None, "reason": None, "error": None}
async def ws_idle():
    hdr = {"X-User-Id": "alice", "X-Terminal-Template-Id": "personal", "X-Session-Id": "chat-1"}
    s, _, b = call("POST", "/api/terminals")
    sid = json.loads(b)["id"]
    async with websockets.connect(f"ws://{NODE}:30080/api/terminals/{sid}?user_id=alice", additional_headers=hdr, proxy=None, close_timeout=2) as c:
        await c.send(json.dumps({"type": "auth", "token": KEY, "chat_id": "chat-1"}))
        idle_ws["open"] = True
        end = time.time() + 420
        try:
            while time.time() < end:
                await c.send('{"type":"ping"}'); idle_ws["pings"] += 1
                until = time.time() + 20
                # Read and discard the output of the shell. A message of the sandbox is not a use.
                while time.time() < until:
                    try: await asyncio.wait_for(c.recv(), max(0.1, until - time.time()))
                    except asyncio.TimeoutError: pass
        except websockets.exceptions.ConnectionClosed as e:
            if e.rcvd is not None: idle_ws["code"], idle_ws["reason"] = e.rcvd.code, e.rcvd.reason
def ws_idle_thread():
    try: asyncio.run(ws_idle())
    except Exception as e: idle_ws["error"] = repr(e)
idle_thread = threading.Thread(target=ws_idle_thread, daemon=True)
idle_thread.start()
for _ in range(150):
    if idle_ws["open"] or idle_ws["error"]: break
    time.sleep(0.1)
check("terminal WebSocket of alice is open, with only keepalive messages", idle_ws["open"], idle_ws)

# ---- stop and start by the API ----
# A stop operation closes the open WebSockets of the sandbox with the code 1012 and a text of warren.
# The pod gets its stop signal only after that, thus the code and the text do not come from the pod.
async def ws_stop():
    hdr = {"X-User-Id": "bob", "X-Terminal-Template-Id": "personal", "X-Session-Id": "chat-1"}
    s, _, b = call("POST", "/api/terminals", user="bob")
    sid = json.loads(b)["id"]
    async with websockets.connect(f"ws://{NODE}:30080/api/terminals/{sid}?user_id=bob", additional_headers=hdr, proxy=None) as c:
        await c.send(json.dumps({"type": "auth", "token": KEY, "chat_id": "chat-1"}))
        await c.send(b"echo open-$((1+1))\n")
        buf = b""
        try:
            while b"open-2" not in buf:
                m = await asyncio.wait_for(c.recv(), 10); buf += m if isinstance(m, bytes) else m.encode()
        except Exception as e: pass
        check("terminal WebSocket of bob is open before the stop", b"open-2" in buf, buf[-120:])
        t0 = time.time()
        stop = await asyncio.to_thread(call, "POST", "/api/v1/terminals/templates/personal/stop", None, {"user_id": "bob", "chat_id": None, "model_id": None})
        code, reason = None, None
        try:
            while True: await asyncio.wait_for(c.recv(), 10)
        except websockets.exceptions.ConnectionClosed as e:
            if e.rcvd is not None: code, reason = e.rcvd.code, e.rcvd.reason
        except Exception as e: pass
        return stop, code, reason, time.time() - t0
(s, _, b), code, reason, dt = asyncio.run(ws_stop())
check("stop operation", s == 200, b)
check("the stop operation closes an open terminal WebSocket with the code 1012", code == 1012 and reason == "An operation stopped or restarted the terminal" and dt < 5, (code, reason, round(dt, 1)))
gone = False
for _ in range(30):
    if not any(x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] == "user:bob" and "deletionTimestamp" not in x["metadata"] for x in pods()): gone = True; break
    time.sleep(1)
check("the pod of bob stops", gone)

# ---- system information of a stopped sandbox ----
# The request for the system information does not start a sandbox. The reply has the text that Open WebUI shows.
t0 = time.time(); s, _, b = call("GET", "/system/info", user="bob"); dt = time.time() - t0
check("system information of a stopped sandbox gives 409 immediately", s == 409 and dt < 5 and json.loads(b) == {"detail": "The terminal is stopped. It starts when you use it."}, (s, round(dt, 1), b))
time.sleep(5)
live = [x["metadata"]["name"] for x in pods() if x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] == "user:bob" and "deletionTimestamp" not in x["metadata"]]
check("the request for the system information starts no sandbox", live == [], live)

# ---- watcher WebSocket of a stopped sandbox ----
# A WebSocket on a watcher route does not start a sandbox. Warren accepts it, and it waits for a start by a different request.
# On a sandbox that operates, the process list comes immediately after the first message.
async def ws_watch():
    hdr = {"X-User-Id": "bob", "X-Terminal-Template-Id": "personal", "X-Session-Id": "chat-1"}
    async with websockets.connect(f"ws://{NODE}:30080/execute?user_id=bob", additional_headers=hdr, proxy=None, close_timeout=2) as c:
        await c.send(json.dumps({"type": "auth", "token": KEY, "chat_id": "chat-1"}))
        got = "nothing"
        try:
            got = ("message", await asyncio.wait_for(c.recv(), 8))
        except asyncio.TimeoutError: pass
        except websockets.exceptions.ConnectionClosed as e: got = ("close", e.rcvd.code if e.rcvd is not None else None)
        return got
got = asyncio.run(ws_watch())
check("a watcher WebSocket of a stopped sandbox waits: no data and no close", got == "nothing", got)
live = [x["metadata"]["name"] for x in pods() if x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] == "user:bob" and "deletionTimestamp" not in x["metadata"]]
check("the watcher WebSocket starts no sandbox", live == [], live)

# ---- metrics ----
s, _, b = call("GET", "/metrics", user=None, key=None, base=ADMIN)
text = b.decode()
check("metrics", s == 200 and "warren_sandbox_cold_start_seconds_count" in text and 'warren_sandboxes{state="ready"}' in text and 'warren_sandbox_reaps_total{reason="api"} 1' in text, [l for l in text.splitlines() if l.startswith("warren_")][:12])
s, _, b = call("GET", "/metrics", user=None)
check("metrics are not on the port for Open WebUI", s != 200 or b"warren_sandboxes" not in b, s)

# ---- idle reaper (idle timeout 90 s) ----
# The terminal WebSocket of alice is open in this time, and it sends its keepalive message each 20 seconds.
print("INFO wait for the idle reaper (about 2.5 minutes)", flush=True)
deadline = time.time() + 240
while time.time() < deadline and len([x for x in pods() if "deletionTimestamp" not in x["metadata"]]) > 0:
    time.sleep(10)
left = [x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] for x in pods() if "deletionTimestamp" not in x["metadata"]]
check("idle sandboxes stop", left == [], left)
idle_thread.join(30)
check("the idle stop closes a terminal WebSocket that sent only keepalive messages with the code 4004", idle_ws["code"] == 4004 and idle_ws["reason"] == "The terminal stopped after its idle time" and idle_ws["pings"] >= 4, idle_ws)
time.sleep(5)
secs = json.loads(kubectl("-n", "warren-sandboxes", "get", "secrets", "-o", "json"))["items"]
check("Secrets are gone with their pods", len(secs) == 0, [x["metadata"]["name"] for x in secs])
s, out = run("echo back-$((1+1))")
check("a request after the stop starts a new sandbox", s == 200 and "back-2" in out, out)

print("FAILED:", fails if fails else "none")
sys.exit(1 if fails else 0)
