# Lifecycle checks for a warren deployment on the kind cluster of setup.sh.
# Start this script with run.sh. The file README.md gives the procedure.
import asyncio, json, subprocess, sys, time, urllib.request, urllib.error
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

s, out = run("echo found=$(env | grep -c 0123456789abcdef); ls /var/run/secrets/kubernetes.io 2>&1 | head -1")
text = "".join(o["data"] for o in json.loads(out)["output"]) if s == 200 else out
check("sandbox has no connection key and no service account token", s == 200 and "found=0" in text and "No such file" in text, text)

# ---- warm path ----
t0 = time.time(); s, _, b = call("GET", "/files/cwd"); warm = time.time() - t0
cwd = json.loads(b).get("cwd") if s == 200 else None
check("warm request", s == 200 and cwd == "/home/user" and warm < 1, (s, b[:80], round(warm, 2)))
s, _, b = call("GET", "/openapi.json")
check("warm tool list is filtered by the sandbox", s == 200 and "/execute" in json.loads(b)["paths"], s)
s, _, b = call("POST", "/files/write", body={"path": "/home/user/secret-of-alice.txt", "content": "alice-private-data\n"})
check("alice writes a file", s == 200, b)

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

# ---- stop and start by the API ----
s, _, b = call("POST", "/api/v1/terminals/templates/personal/stop", user=None, body={"user_id": "bob", "chat_id": None, "model_id": None})
check("stop operation", s == 200, b)
gone = False
for _ in range(30):
    if not any(x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] == "user:bob" and "deletionTimestamp" not in x["metadata"] for x in pods()): gone = True; break
    time.sleep(1)
check("the pod of bob stops", gone)

# ---- metrics ----
s, _, b = call("GET", "/metrics", user=None, key=None, base=ADMIN)
text = b.decode()
check("metrics", s == 200 and "warren_sandbox_cold_start_seconds_count" in text and 'warren_sandboxes{state="ready"}' in text and 'warren_sandbox_reaps_total{reason="api"} 1' in text, [l for l in text.splitlines() if l.startswith("warren_")][:12])
s, _, b = call("GET", "/metrics", user=None)
check("metrics are not on the port for Open WebUI", s != 200 or b"warren_sandboxes" not in b, s)

# ---- idle reaper (idle timeout 90 s) ----
print("INFO wait for the idle reaper (about 2.5 minutes)", flush=True)
deadline = time.time() + 240
while time.time() < deadline and len([x for x in pods() if "deletionTimestamp" not in x["metadata"]]) > 0:
    time.sleep(10)
left = [x["metadata"]["annotations"]["warren.coderpunch.io/context-id"] for x in pods() if "deletionTimestamp" not in x["metadata"]]
check("idle sandboxes stop", left == [], left)
time.sleep(5)
secs = json.loads(kubectl("-n", "warren-sandboxes", "get", "secrets", "-o", "json"))["items"]
check("Secrets are gone with their pods", len(secs) == 0, [x["metadata"]["name"] for x in secs])
s, out = run("echo back-$((1+1))")
check("a request after the stop starts a new sandbox", s == 200 and "back-2" in out, out)

print("FAILED:", fails if fails else "none")
sys.exit(1 if fails else 0)
