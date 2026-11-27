# Storage and policy checks for a warren deployment on the kind cluster of setup.sh.
# Start this script with run.sh. The file README.md gives the procedure.
import json, subprocess, sys, time, urllib.request, urllib.error

NODE = subprocess.run(["docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "warren-dev-control-plane"], capture_output=True, text=True).stdout.strip()
W = f"http://{NODE}:30080"
ADMIN = f"http://{NODE}:30090"
KEY = "connection-key-0123456789abcdef0123456789"
P = "warren.coderpunch.io/"
fails = []
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def check(name, cond, extra=""):
    print(("PASS " if cond else "FAIL ") + name + (" " + str(extra)[:300] if (not cond or extra) else ""), flush=True)
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

def items(kind):
    return json.loads(kubectl("-n", "warren-sandboxes", "get", kind, "-o", "json"))["items"]

def pods(): return [x for x in items("pods") if "deletionTimestamp" not in x["metadata"]]
def pvcs(): return items("pvc")
def ctx(x): return x["metadata"]["annotations"].get(P + "context-id")
def find(objs, context, template=None):
    return [x for x in objs if ctx(x) == context and (template is None or x["metadata"]["labels"][P + "template"] == template)]

def run(cmd, user="alice", **kw):
    for _ in range(3):
        s, _, b = call("POST", "/execute?wait=25", user=user, body={"command": cmd}, **kw)
        if s != 503: break
        time.sleep(2)
    if s != 200: return s, b.decode()
    return s, "".join(o["data"] for o in json.loads(b).get("output", []))

def mgmt(method, path, body=None, timeout=170):
    return call(method, "/api/v1/terminals/templates/" + path, user=None, body=body, timeout=timeout)

def wait(what, cond, seconds=60):
    end = time.time() + seconds
    while time.time() < end:
        if cond(): return True
        time.sleep(1)
    return False

# ---- templates show the policy settings ----
s, _, b = mgmt("GET", "tuned")
d = json.loads(b)["data"]
check("template shows image, limits, and storage", s == 200 and d["cpu_limit"] == "0.5" and d["memory_limit"] == "512Mi" and d["storage"] == "200Mi" and "open-terminal" in d["image"] and d["env"] is None, d)

# ---- a persistent sandbox: files survive a stop ----
check("clean start: no pods, no volumes", len(pods()) == 0 and len(pvcs()) == 0, (len(pods()), len(pvcs())))
t0 = time.time()
s, out = run("echo kept-data > /home/user/kept.txt; echo temp > /tmp/temp.txt; id -u; id -G; pwd; df -h /home/user | tail -1")
check("first command in a sandbox with a volume", s == 200, out)
print(f"INFO cold start with a new volume: {time.time()-t0:.1f} s; {out!r}", flush=True)
v = find(pvcs(), "user:alice", "personal")
check("one volume for alice", len(v) == 1 and v[0]["status"]["phase"] == "Bound" and v[0]["spec"]["resources"]["requests"]["storage"] == "1Gi", [x["metadata"]["name"] for x in pvcs()])
pod = find(pods(), "user:alice", "personal")[0]
check("the volume has the name of the sandbox and no owner", v[0]["metadata"]["name"] == pod["metadata"]["name"] and "ownerReferences" not in v[0]["metadata"])
vol_uid = v[0]["metadata"]["uid"]
home = [x for x in pod["spec"]["volumes"] if x["name"] == "home"][0]
check("the pod mounts the volume as home", home.get("persistentVolumeClaim", {}).get("claimName") == pod["metadata"]["name"], home)
lim = pod["spec"]["containers"][0]["resources"]
check("default resource limits are in the pod", lim["limits"]["memory"] == "2Gi" and lim["limits"]["cpu"] == "2" and lim["requests"]["cpu"] == "100m", lim)

s, _, b = mgmt("POST", "personal/stop", {"user_id": "alice", "chat_id": None, "model_id": None})
check("stop operation", s == 200, b)
check("the pod stops", wait("stop", lambda: not find(pods(), "user:alice", "personal") and not [x for x in items("pods") if ctx(x) == "user:alice"]))
check("the volume stays after the stop", [x["metadata"]["uid"] for x in find(pvcs(), "user:alice", "personal")] == [vol_uid])
s, _, b = mgmt("GET", "personal/instances")
inst = {i["context_id"]: i["status"] for i in json.loads(b)}
check("a stopped sandbox with a volume is in the instance list", inst.get("user:alice") == "stopped", inst)

t0 = time.time()
s, out = run("cat /home/user/kept.txt; ls /tmp/temp.txt 2>&1")
check("after a new start, the home directory has the file", s == 200 and "kept-data" in out, out)
check("the temporary directory is new", "No such file" in out, out)
print(f"INFO start with an existing volume: {time.time()-t0:.1f} s", flush=True)
check("same volume after the start", [x["metadata"]["uid"] for x in find(pvcs(), "user:alice", "personal")] == [vol_uid])

# ---- isolation: bob has a different volume ----
s, out = run("cat /home/user/kept.txt 2>&1; echo bob-data > /home/user/bob.txt; ls /home/user", user="bob")
check("bob has a different volume", s == 200 and "kept-data" not in out and "bob.txt" in out, out)
check("two volumes", len(pvcs()) == 2, len(pvcs()))

# ---- restart keeps the files ----
s, _, b = mgmt("POST", "personal/restart", {"user_id": "alice", "chat_id": None, "model_id": None})
check("restart reports running", s == 200 and json.loads(b).get("status") == "running", b)
s, out = run("cat /home/user/kept.txt")
check("restart keeps the files", "kept-data" in out, out)

# ---- reset deletes the files and gives the volume back ----
s, _, b = mgmt("POST", "personal/reset", {"user_id": "alice", "chat_id": None, "model_id": None})
check("reset operation", s == 200 and json.loads(b).get("status") == "stopped", b)
nv = find(pvcs(), "user:alice", "personal")
check("reset removed the pod and replaced the volume with an empty volume", not [x for x in items("pods") if ctx(x) == "user:alice" and x["metadata"]["labels"][P + "template"] == "personal"] and len(nv) == 1 and nv[0]["metadata"]["uid"] != vol_uid, [x["metadata"]["name"] for x in pvcs()])
check("the new volume stays on the account of alice", nv and nv[0]["metadata"]["annotations"].get(P + "created-by") == "alice", nv and nv[0]["metadata"]["annotations"])
s, _, b = mgmt("GET", "personal/instances")
inst = {i["context_id"]: i["status"] for i in json.loads(b)}
check("after a reset, the instance stays in the list as stopped", inst.get("user:alice") == "stopped", inst)
s, out = run("ls -A /home/user/kept.txt 2>&1; echo after-reset")
check("after a reset, the home directory is empty", s == 200 and "No such file" in out and "after-reset" in out, out)
nv2 = find(pvcs(), "user:alice", "personal")
check("the next start uses the empty volume of the reset", len(nv2) == 1 and nv and nv2[0]["metadata"]["uid"] == nv[0]["metadata"]["uid"] and nv2[0]["status"]["phase"] == "Bound")
s, out = run("cat /home/user/bob.txt", user="bob")
check("the reset of alice did not change the volume of bob", "bob-data" in out, out)

# ---- "delete instance" removes the files (Open WebUI: "The instance and its files are deleted") ----
s, out = run("echo carol-data > /home/user/c.txt; stat -c '%u:%g %a' /home/user", user="carol")
check("carol has a sandbox with a volume", s == 200 and len(find(pvcs(), "user:carol", "personal")) == 1, out)
s, _, b = mgmt("DELETE", "personal/instance", {"user_id": "carol", "chat_id": None, "model_id": None})
check("delete instance", s == 200 and json.loads(b).get("status") == "stopped", b)
check("delete removed the pod and the volume", not [x for x in items("pods") if ctx(x) == "user:carol"] and not find(pvcs(), "user:carol", "personal"))
s, _, b = mgmt("GET", "personal/instances")
check("a deleted instance is not in the list", "user:carol" not in [i["context_id"] for i in json.loads(b)], b)
s, out = run("ls /home/user/c.txt 2>&1", user="carol")
check("after a delete, the next start has an empty home", s == 200 and "No such file" in out, out)

# ---- a volume that a person deletes under a running sandbox ----
cname = find(pvcs(), "user:carol", "personal")[0]["metadata"]["name"]
cuid = find(pvcs(), "user:carol", "personal")[0]["metadata"]["uid"]
kubectl("-n", "warren-sandboxes", "delete", "pvc", cname, "--wait=false")
check("warren stops the pod whose volume is in deletion", wait("volume_gone", lambda: not [x for x in items("pods") if ctx(x) == "user:carol"], 90))
s, out = run("echo carol-again", user="carol")
nc = find(pvcs(), "user:carol", "personal")
check("the next start makes a new volume", s == 200 and "carol-again" in out and len(nc) == 1 and nc[0]["metadata"]["uid"] != cuid and "deletionTimestamp" not in nc[0]["metadata"], out)
mgmt("DELETE", "personal/instance", {"user_id": "carol", "chat_id": None, "model_id": None})

# ---- ephemeral chat sandbox ----
s, out = run("echo chat-data > /home/user/c.txt; stat -c 'own=%u:%g mode=%a' /home/user", template="by-chat", chat="chat-1")
check("chat sandbox starts", s == 200, out)
check("Kubernetes gives the home directory to the group of the sandbox (fsGroup)", "own=0:1001" in out and "mode=2777" in out, out)
cp = find(pods(), "chat:chat-1", "by-chat")[0]
h = [x for x in cp["spec"]["volumes"] if x["name"] == "home"][0]
check("chat sandbox has an emptyDir home with a size limit and no volume", h.get("emptyDir", {}).get("sizeLimit") == "1Gi" and not find(pvcs(), "chat:chat-1"), h)
check("ephemeral storage limit includes the home directory", cp["spec"]["containers"][0]["resources"]["limits"]["ephemeral-storage"] == "3Gi", cp["spec"]["containers"][0]["resources"])
mgmt("POST", "by-chat/stop", {"user_id": "alice", "chat_id": "chat-1", "model_id": None})
wait("stop", lambda: not [x for x in items("pods") if ctx(x) == "chat:chat-1"])
s, out = run("ls /home/user/c.txt 2>&1", template="by-chat", chat="chat-1")
check("an ephemeral home is empty after a stop", s == 200 and "No such file" in out, out)
mgmt("POST", "by-chat/stop", {"user_id": "alice", "chat_id": "chat-1", "model_id": None})

# ---- policy with env, resources, and a user list ----
s, out = run("echo G=$GREETING; echo L=$LITERAL; echo S=$SHARED_SETTING; echo T=$OPEN_TERMINAL_EXECUTE_TIMEOUT", template="tuned")
check("policy variables are in the sandbox", s == 200 and "G=hello from the policy" in out and "S=from-defaults" in out, out)
check("a variable with $(...) stays as text and has no key", "L=$(OPEN_TERMINAL_API_KEY) $HOME" in out, out)
tp = find(pods(), "user:alice", "tuned")[0]
r = tp["spec"]["containers"][0]["resources"]
check("policy resources are in the pod", r["limits"]["cpu"] == "500m" and r["limits"]["memory"] == "512Mi" and r["requests"]["memory"] == "128Mi" and r["limits"]["ephemeral-storage"] == "700Mi", r)
check("idle time of the policy is on the pod", tp["metadata"]["annotations"].get(P + "idle-timeout") == "45s", tp["metadata"]["annotations"])
s, _, b = call("GET", "/files/cwd", user="bob", template="tuned")
check("a user that is not in the list gets 403", s == 403 and b"detail" in b, (s, b))
s, _, b = call("GET", "/openapi.json", user="bob", template="tuned")
check("a user that is not in the list gets no tool list", s == 403, s)
check("no sandbox for the user that is not in the list", not find(pods(), "user:bob", "tuned"))
s, _, b = mgmt("POST", "tuned/start", {"user_id": "bob", "chat_id": None, "model_id": None})
check("a manager cannot start the sandbox for that user", s == 403, (s, b))

# ---- volume limit for chats (2 for each user) ----
for c in ("k1", "k2"):
    s, out = run("echo x", template="chat-kept", chat=c)
    check(f"chat volume {c}", s == 200, out)
    mgmt("POST", "chat-kept/stop", {"user_id": "alice", "chat_id": c, "model_id": None})
s, _, b = call("POST", "/execute?wait=5", body={"command": "echo x"}, template="chat-kept", chat="k3")
check("third chat volume is refused with 429", s == 429 and b"saved files" in b, (s, b))
check("no volume for the refused chat", not find(pvcs(), "chat:k3"))
s, out = run("echo still-ok", user="alice")
check("the personal sandbox is not blocked by the chat volumes", s == 200 and "still-ok" in out, out)
s, _, b = mgmt("POST", "chat-kept/reset", {"user_id": "manager", "chat_id": "k1", "model_id": None})
check("a reset keeps the chat instance and its place in the limit", s == 200 and len(find(pvcs(), "chat:k1")) == 1, b)
s, _, b = call("POST", "/execute?wait=5", body={"command": "echo x"}, template="chat-kept", chat="k3")
check("after a reset, a new chat volume is still refused", s == 429, (s, b))
s, _, b = mgmt("DELETE", "chat-kept/instance", {"user_id": "manager", "chat_id": "k1", "model_id": None})
check("a delete gives a chat volume back", s == 200 and not find(pvcs(), "chat:k1"), b)
s, _, b = mgmt("GET", "chat-kept/instances")
check("stopped chat sandboxes with a volume are in the instance list", sorted(i["context_id"] for i in json.loads(b)) == ["chat:k2"] and json.loads(b)[0]["status"] == "stopped", b)
s, out = run("echo x", template="chat-kept", chat="k3")
check("after the delete, a new chat volume is possible", s == 200, out)
for c in ("k2", "k3"):
    mgmt("DELETE", "chat-kept/instance", {"user_id": "manager", "chat_id": c, "model_id": None})

# ---- metrics ----
s, _, b = call("GET", "/metrics", user=None, key=None, base=ADMIN)
text = b.decode()
check("volume metrics", "warren_volumes " in text and 'warren_volume_deletes_total{reason="api"}' in text, [l for l in text.splitlines() if l.startswith("warren_vol")])

# ---- idle time of the policy (45 s) and retention (2 m) ----
print("INFO wait for the idle time of the tuned policy (45 s)", flush=True)
run("echo touch", template="tuned"); run("echo touch")
check("the tuned sandbox stops after its own idle time", wait("idle", lambda: not find(pods(), "user:alice", "tuned"), 120))
check("a sandbox with the idle time of warren (90 s) still operates", len(find(pods(), "user:alice", "personal")) == 1)
print("INFO wait for the idle stop of all sandboxes and for the unused mark", flush=True)
check("all sandboxes stop", wait("idle", lambda: len(pods()) == 0, 240))
check("volumes get the unused mark", wait("mark", lambda: all(P + "unused-since" in x["metadata"]["annotations"] for x in find(pvcs(), None) or pvcs()) and len(pvcs()) >= 2, 90), [x["metadata"]["annotations"] for x in pvcs()])
marks = {ctx(x): x["metadata"]["annotations"].get(P + "unused-since") for x in pvcs()}
# bob comes back: his volume loses the mark and survives.
s, out = run("cat /home/user/bob.txt", user="bob")
check("bob comes back before the retention time", "bob-data" in out, out)
bv = find(pvcs(), "user:bob", "personal")
check("a start removes the unused mark", len(bv) == 1 and P + "unused-since" not in bv[0]["metadata"]["annotations"], bv and bv[0]["metadata"]["annotations"])
# a restart of warren must not reset the retention clock of alice.
kubectl("-n", "warren-system", "rollout", "restart", "deploy/warren")
kubectl("-n", "warren-system", "rollout", "status", "deploy/warren", "--timeout=90s")
av = find(pvcs(), "user:alice", "personal")
check("the mark survives a restart of warren", len(av) == 1 and av[0]["metadata"]["annotations"].get(P + "unused-since") == marks.get("user:alice"), (av and av[0]["metadata"]["annotations"], marks))
print("INFO wait for the retention time (2 m)", flush=True)
check("the unused volume of alice is deleted after the retention time", wait("retention", lambda: not find(pvcs(), "user:alice", "personal"), 240))
check("the volume of bob stays while his sandbox operates or was used", len(find(pvcs(), "user:bob", "personal")) == 1)
pvs = json.loads(kubectl("get", "pv", "-o", "json"))["items"]
check("the cluster removed the storage of deleted volumes", len([x for x in pvs if x["status"]["phase"] == "Bound"]) == len(pvcs()), [(x["metadata"]["name"], x["status"]["phase"]) for x in pvs])
log = kubectl("-n", "warren-system", "logs", "deploy/warren")
check("audit record for the deleted volume", '"msg":"volume deleted"' in log and '"reason":"retention"' in log, log[-300:])

# ---- policy change with no restart ----
warren_pod = kubectl("-n", "warren-system", "get", "pods", "-l", "app=warren", "-o", "jsonpath={.items[*].metadata.name}")
s, out = run("echo team-data > /home/user/t.txt", template="team")
check("shared sandbox with a volume", s == 200 and len(find(pvcs(), "default", "team")) == 1, out)
cm = json.loads(kubectl("-n", "warren-system", "get", "cm", "warren-policies", "-o", "json"))
text = cm["data"]["policies.yaml"]
new = text.replace('users: ["alice"]', 'users: ["alice", "bob"]')
a = new.index("  - id: team"); b2 = new.index("  - id: chat-kept")
new = new[:a] + new[b2:]
assert new != text and "id: team" not in new
patch = json.dumps({"data": {"policies.yaml": new}})
subprocess.run(["docker", "exec", "warren-dev-control-plane", "kubectl", "-n", "warren-system", "patch", "cm", "warren-policies", "--type=merge", "-p", patch], capture_output=True)
print("INFO wait until Kubernetes and warren read the new policy file (up to 3 minutes)", flush=True)
def bob_ok():
    s, _, _ = call("GET", "/openapi.json", user="bob", template="tuned", timeout=10)
    return s == 200
check("the new user list is in use with no restart", wait("reload", bob_ok, 200))
check("warren did not start again", kubectl("-n", "warren-system", "get", "pods", "-l", "app=warren", "-o", "jsonpath={.items[*].metadata.name}") == warren_pod)
s, _, b = mgmt("GET", "team")
check("the removed policy is not a template", s == 404, s)
s, _, b = call("GET", "/files/cwd", template="team")
check("a request for the removed policy gets 404", s == 404, (s, b))
check("warren stops the pod of the removed policy", wait("no_policy", lambda: not [x for x in items("pods") if ctx(x) == "default"], 90))
check("warren keeps the volume of the removed policy", len(find(pvcs(), "default", "team")) == 1)
kubectl("-n", "warren-sandboxes", "delete", "pvc", find(pvcs(), "default", "team")[0]["metadata"]["name"])

print("FAILED:", fails if fails else "none")
sys.exit(1 if fails else 0)
