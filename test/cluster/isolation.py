# Isolation checks for a warren deployment on the kind cluster of setup.sh.
# Start this script with run.sh. The file README.md gives the procedure.
import json, os, subprocess, sys, time, urllib.request, urllib.error

NODE = subprocess.run(["docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "warren-dev-control-plane"], capture_output=True, text=True).stdout.strip()
W = f"http://{NODE}:30080"
KEY = "connection-key-0123456789abcdef0123456789"
P = "warren.coderpunch.io/"
IMG = "ghcr.io/open-webui/open-terminal:0.18.8-openshift"
PUBLIC_OK, PUBLIC_BLOCKED = "203.0.113.10", "203.0.113.20"
fails = []
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def check(name, cond, extra=""):
    print(("PASS " if cond else "FAIL ") + name + (" " + str(extra)[:300] if (not cond or extra) else ""), flush=True)
    if not cond: fails.append(name)

def sh(*args, stdin=None):
    return subprocess.run(list(args), capture_output=True, text=True, input=stdin)

def kubectl(*args, stdin=None):
    r = sh("docker", "exec", "-i", "warren-dev-control-plane", "kubectl", *args, stdin=stdin)
    return r.stdout + (r.stderr if r.returncode else "")

def call(method, path, user="alice", body=None, template="personal", timeout=70):
    h = {"Authorization": "Bearer " + KEY}
    if user:
        h["X-User-Id"] = user; h["X-Terminal-Template-Id"] = template; h["X-Session-Id"] = "s1"
    if body is not None: h["Content-Type"] = "application/json"; body = json.dumps(body).encode()
    req = urllib.request.Request(W + path, data=body, method=method, headers=h)
    try:
        r = opener.open(req, timeout=timeout)
        return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return 0, str(e).encode()

def run(cmd, user="alice", template="personal"):
    for _ in range(4):
        s, b = call("POST", "/execute?wait=28", user=user, body={"command": cmd}, template=template)
        if s not in (503, 0): break
        time.sleep(3)
    if s != 200: return s, b.decode()[:300]
    return s, "".join(o["data"] for o in json.loads(b).get("output", []))

def probe(host, port, user="alice", template="personal"):
    """open / timeout / refused, as seen from inside a sandbox."""
    code = ("import socket\ns=socket.socket(socket.AF_INET6 if ':' in '%s' else socket.AF_INET); s.settimeout(3)\n"
            "try:\n s.connect(('%s',%d)); print('RESULT open')\nexcept socket.timeout: print('RESULT timeout')\n"
            "except ConnectionRefusedError: print('RESULT refused')\nexcept Exception as e: print('RESULT error', e)") % (host, host, port)
    s, out = run("python3 -c \"%s\"" % code, user=user, template=template)
    for line in out.splitlines():
        if "RESULT" in line: return line.split("RESULT", 1)[1].strip()
    return "no-result %s %s" % (s, out[:120])

def resolve(name, user="alice", template="personal"):
    s, out = run("python3 -c \"import socket; socket.setdefaulttimeout(4)\ntry: print('RESULT', socket.gethostbyname('%s'))\nexcept Exception as e: print('RESULT fail')\"" % name, user=user, template=template)
    for line in out.splitlines():
        if "RESULT" in line: return line.split("RESULT", 1)[1].strip()
    return "no-result"

def items(kind, ns="warren-sandboxes"):
    return json.loads(kubectl("-n", ns, "get", kind, "-o", "json"))["items"]

def sandbox_pod(context, template):
    for x in items("pods"):
        if x["metadata"]["annotations"].get(P + "context-id") == context and x["metadata"]["labels"].get(P + "template") == template and "deletionTimestamp" not in x["metadata"]:
            return x
    return None

def wait(cond, seconds=60, step=2):
    end = time.time() + seconds
    while time.time() < end:
        if cond(): return True
        time.sleep(step)
    return False

# ---------- setup: a target outside the sandboxes, and two "public" addresses on the node ----------
for ip in (PUBLIC_OK, PUBLIC_BLOCKED):
    sh("docker", "exec", "warren-dev-control-plane", "ip", "addr", "add", ip + "/32", "dev", "lo")
listener = "import http.server,socketserver,sys\nsocketserver.TCPServer.allow_reuse_address=True\nhttp.server.ThreadingHTTPServer((sys.argv[1],int(sys.argv[2])),http.server.SimpleHTTPRequestHandler).serve_forever()"
def target(name, host_network, binds):
    cmds = " & ".join("python3 -c \"$CODE\" %s %d" % (ip, port) for ip, port in binds) + " & wait"
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": "wtarget", "labels": {"app": name}},
            "spec": {"hostNetwork": host_network, "containers": [{"name": "c", "image": IMG, "command": ["sh", "-c", cmds],
                     "env": [{"name": "CODE", "value": listener}], "workingDir": "/tmp"}]}}
objs = [{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "wtarget"}},
        target("inside", False, [("0.0.0.0", 8000), ("0.0.0.0", 9000)]),
        target("onnode", True, [(PUBLIC_OK, 18080), (PUBLIC_BLOCKED, 18080)])]
kubectl("apply", "-f", "-", stdin=json.dumps({"apiVersion": "v1", "kind": "List", "items": objs}))
check("setup: target pods run", wait(lambda: all(p["status"].get("phase") == "Running" for p in items("pods", "wtarget")) and len(items("pods", "wtarget")) == 2, 90))
TARGET = [p for p in items("pods", "wtarget") if p["metadata"]["name"] == "inside"][0]["status"]["podIP"]

manifest = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "manifests.yaml")).read()
extra = f'''      - id: closed
        name: No network
        scope: user
        storage: {{type: ephemeral}}
        network: {{egress: none}}
      - id: database
        name: One destination
        scope: user
        storage: {{type: ephemeral}}
        network:
          egress: none
          dns: true
          allow:
            - {{cidr: {TARGET}/32, ports: [8000]}}
      - id: userns
        name: User namespace
        scope: user
        userNamespace: true
        storage: {{size: 100Mi, retention: 1h}}
      - id: alpine
        name: Image with a different layout
        scope: user
        image: ghcr.io/open-webui/open-terminal:0.18.8-alpine
        command: ["/sbin/tini", "--", "/usr/local/bin/python3", "-I", "-u", "-m", "open_terminal"]
        storage: {{type: ephemeral}}
      - id: wrong-command
        name: Command that is not in the image
        scope: user
        command: ["/no/such/program"]
        storage: {{type: ephemeral}}
      - id: slim
        name: Other image
        scope: user
        image: ghcr.io/open-webui/open-terminal:0.18.8-slim
        runAsUser: 1000
        storage: {{type: ephemeral}}
'''
marker = "      - id: tuned\n"
assert manifest.count(marker) == 1
manifest = manifest.replace(marker, extra + marker)
arg = "        - --warren-pod-labels=app=warren\n"
assert manifest.count(arg) == 1
manifest = manifest.replace(arg, arg + f"        - --egress-blocked-cidrs={PUBLIC_BLOCKED}/32\n")
limit = "        - --max-sandboxes-per-user=4\n"
assert manifest.count(limit) == 1
manifest = manifest.replace(limit, "        - --max-sandboxes-per-user=10\n")
kubectl("-n", "warren-sandboxes", "delete", "pods,pvc", "--all", "--wait=true", "--timeout=90s")
kubectl("apply", "-f", "-", stdin=manifest)
kubectl("-n", "warren-system", "rollout", "restart", "deploy/warren")
kubectl("-n", "warren-system", "rollout", "status", "deploy/warren", "--timeout=120s")
time.sleep(5)

WARREN_IP = items("pods", "warren-system")[0]["status"]["podIP"]
WARREN_SVC = json.loads(kubectl("-n", "warren-system", "get", "svc", "warren", "-o", "json"))["spec"]["clusterIP"]
API_SVC = json.loads(kubectl("-n", "default", "get", "svc", "kubernetes", "-o", "json"))["spec"]["clusterIP"]
DNS_SVC = json.loads(kubectl("-n", "kube-system", "get", "svc", "kube-dns", "-o", "json"))["spec"]["clusterIP"]

# ---------- two users ----------
s, out = run("echo secret-of-alice > /home/user/a.txt; id -u; cat /proc/1/cmdline | tr '\\0' ' '")
check("alice has a sandbox", s == 200 and "1001" in out, out)
check("the server starts with the fixed command, not through the entry point", "/usr/bin/tini -- /usr/local/bin/python3 -I -u -m open_terminal run" in out, out)
s, out = run("cat /home/user/a.txt 2>&1; echo bob-here", user="bob")
check("bob has a sandbox and does not see the file of alice", s == 200 and "bob-here" in out and "secret-of-alice" not in out, out)
pa, pb = sandbox_pod("user:alice", "personal"), sandbox_pod("user:bob", "personal")
IPA, IPB = pa["status"]["podIP"], pb["status"]["podIP"]

# ---------- pod security ----------
sc, cs = pa["spec"]["securityContext"], pa["spec"]["containers"][0]["securityContext"]
check("pod: runs as a user that is not root, with the default seccomp profile", sc.get("runAsNonRoot") is True and sc.get("seccompProfile", {}).get("type") == "RuntimeDefault", sc)
check("pod: a user ID and a group ID that are numbers and not 0", sc.get("runAsUser") == 1001 and sc.get("runAsGroup") == 1001, sc)
check("container: no privilege escalation, no capabilities, read-only root", cs.get("allowPrivilegeEscalation") is False and cs.get("capabilities", {}).get("drop") == ["ALL"] and cs.get("readOnlyRootFilesystem") is True, cs)
s, out = run("touch /etc/x 2>&1; touch /usr/local/bin/x 2>&1; touch /home/user/ok && echo home-ok; touch /tmp/ok && echo tmp-ok; grep -E 'CapEff|NoNewPrivs|Seccomp:' /proc/self/status")
check("the sandbox can write only to home and tmp", out.count("Read-only file system") == 2 and "home-ok" in out and "tmp-ok" in out, out)
check("processes have no capabilities, cannot get new privileges, and have a seccomp filter", "CapEff:\t0000000000000000" in out and "NoNewPrivs:\t1" in out and "Seccomp:\t2" in out, out)
s, out = run("ls /var/run/secrets/kubernetes.io 2>&1; env | grep -c -i -E 'connection-key|0123456789abcdef'")
check("no service account token and no connection key in the sandbox", "No such file" in out and out.strip().endswith("0"), out)
ns = json.loads(kubectl("get", "ns", "warren-sandboxes", "-o", "json"))["metadata"]["labels"]
check("the sandbox namespace enforces the restricted Pod Security Standard", ns.get("pod-security.kubernetes.io/enforce") == "restricted", ns)
bad = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "privileged", "namespace": "warren-sandboxes"},
       "spec": {"containers": [{"name": "c", "image": IMG, "command": ["sleep", "100"], "securityContext": {"privileged": True}}]}}
out = kubectl("apply", "-f", "-", stdin=json.dumps(bad))
check("Kubernetes refuses a privileged pod in the sandbox namespace", "violates PodSecurity" in out, out)

# ---------- network: what a sandbox cannot reach ----------
check("alice cannot connect to the sandbox of bob", probe(IPB, 8000) == "timeout", probe(IPB, 8000))
check("bob cannot connect to the sandbox of alice", probe(IPA, 8000, user="bob") == "timeout")
check("alice cannot connect to warren (pod, port for Open WebUI)", probe(WARREN_IP, 8080) == "timeout")
check("alice cannot connect to warren (pod, admin port)", probe(WARREN_IP, 9090) == "timeout")
check("alice cannot connect to warren (service address)", probe(WARREN_SVC, 8080) == "timeout")
check("alice cannot connect to the Kubernetes API (service address)", probe(API_SVC, 443) == "timeout")
check("alice cannot connect to the Kubernetes API (node address)", probe(NODE, 6443) == "timeout")
check("alice cannot connect to the kubelet", probe(NODE, 10250) == "timeout")
check("alice cannot connect to a pod in a different namespace", probe(TARGET, 8000) == "timeout")
check("alice cannot connect to the metadata address of a cloud", probe("169.254.169.254", 80) == "timeout")
check("alice cannot connect to the DNS server on a port that is not 53", probe(DNS_SVC, 9153) == "timeout", probe(DNS_SVC, 9153))

# ---------- network: what the internet mode permits ----------
check("alice can use the DNS of the cluster", resolve("kubernetes.default.svc.cluster.local") == API_SVC, resolve("kubernetes.default.svc.cluster.local"))
check("alice can connect to a public address", probe(PUBLIC_OK, 18080) == "open", probe(PUBLIC_OK, 18080))
check("alice cannot connect to a public address that the administrator blocked", probe(PUBLIC_BLOCKED, 18080) == "timeout", probe(PUBLIC_BLOCKED, 18080))

# ---------- network: nobody but warren reaches a sandbox ----------
def from_pod(ns, pod, host, port):
    code = "import socket\ns=socket.socket(); s.settimeout(3)\ntry:\n s.connect(('%s',%d)); print('open')\nexcept socket.timeout: print('timeout')\nexcept Exception as e: print('refused')" % (host, port)
    return kubectl("-n", ns, "exec", pod, "--", "python3", "-c", code).strip().splitlines()[-1]
check("a pod in a different namespace cannot connect to a sandbox", from_pod("wtarget", "inside", IPA, 8000) == "timeout", from_pod("wtarget", "inside", IPA, 8000))
rogue = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "rogue", "namespace": "warren-sandboxes", "labels": {"app": "warren"}},
         "spec": {"securityContext": {"runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
                  "containers": [{"name": "c", "image": IMG, "command": ["sleep", "100000"],
                                  "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}}}]}}
kubectl("apply", "-f", "-", stdin=json.dumps(rogue))
wait(lambda: any(p["metadata"]["name"] == "rogue" and p["status"].get("phase") == "Running" for p in items("pods")), 60)
check("a pod in the sandbox namespace with the labels of warren cannot connect to a sandbox", from_pod("warren-sandboxes", "rogue", IPA, 8000) == "timeout")
check("a pod in the sandbox namespace with no policy of its own can connect to nothing", from_pod("warren-sandboxes", "rogue", API_SVC, 443) == "timeout" and from_pod("warren-sandboxes", "rogue", PUBLIC_OK, 18080) == "timeout")
kubectl("-n", "warren-sandboxes", "delete", "pod", "rogue", "--wait=false")
s, _ = call("GET", "/files/cwd")
check("warren can connect to the sandbox", s == 200, s)

# ---------- policies with other network settings ----------
s, out = run("echo closed-ok", template="closed")
check("a sandbox with no network starts", s == 200 and "closed-ok" in out, out)
check("no network: no public address", probe(PUBLIC_OK, 18080, template="closed") == "timeout")
check("no network: no DNS", resolve("kubernetes.default.svc.cluster.local", template="closed") == "fail", resolve("kubernetes.default.svc.cluster.local", template="closed"))
check("no network: no sandbox of a different user", probe(IPB, 8000, template="closed") == "timeout")
s, out = run("echo db-ok", template="database")
check("a sandbox with one permitted destination starts", s == 200 and "db-ok" in out, out)
check("permitted destination and port: open", probe(TARGET, 8000, template="database") == "open", probe(TARGET, 8000, template="database"))
check("permitted destination, different port: blocked", probe(TARGET, 9000, template="database") == "timeout")
check("one destination only: no public address", probe(PUBLIC_OK, 18080, template="database") == "timeout")
check("one destination with dns true: DNS operates", resolve("kubernetes.default.svc.cluster.local", template="database") == API_SVC)

# ---------- a different image ----------
s, out = run("id -u; cat /proc/1/cmdline | tr '\\0' ' '; touch /etc/x 2>&1", template="slim")
check("the slim image operates with a user ID from the policy", s == 200 and out.startswith("1000") and "open_terminal run" in out and "Read-only" in out, out)

s, out = run("id -u; cat /proc/1/cmdline | tr '\\0' ' '", template="alpine")
check("the alpine image operates with a command from the policy", s == 200 and out.startswith("1001") and "/sbin/tini" in out, out)
s, _ = call("GET", "/files/cwd", template="wrong-command")
check("a sandbox whose command is not in the image does not become ready", s == 503, s)
check("warren writes the cause to its log", wait(lambda: "the container of a sandbox cannot start" in kubectl("-n", "warren-system", "logs", "deploy/warren") , 60))
s, out = run("id; cat /proc/self/uid_map", template="userns")
check("a policy with a user namespace: the user IDs of the sandbox are other user IDs on the node", s == 200 and "uid=1001 gid=1001" in out and " 0 4294967295" not in out.replace("\t", " "), out)
s, out = run("echo kept > /home/user/k.txt; ls -ln /home/user/k.txt", template="userns")
check("a user namespace operates with a volume", s == 200 and "k.txt" in out, out)

# ---------- files in a persistent home cannot replace the server ----------
attack = r'''mkdir -p ~/.local/bin ~/.local/lib/python3.12/site-packages ~/.config/open-terminal
for n in open-terminal python3 python tini mkdir bash sh; do printf '#!/bin/sh\necho hijack-$0 >> /home/user/HIJACK\nexec /bin/false\n' > ~/.local/bin/$n; chmod +x ~/.local/bin/$n; done
printf 'import os\nopen("/home/user/HIJACK","a").write("sitecustomize " + open("/proc/self/cmdline").read().replace(chr(0)," ") + "\\n")\n' > ~/.local/lib/python3.12/site-packages/sitecustomize.py
cp ~/.local/lib/python3.12/site-packages/sitecustomize.py ~/.local/lib/python3.12/site-packages/usercustomize.py
printf 'host = "127.0.0.1"\nport = 1\napi_key = "chosen-by-the-user"\n' > ~/.config/open-terminal/config.toml
echo 'echo hijack-profile >> /home/user/HIJACK' >> ~/.profile; echo 'echo hijack-bashrc >> /home/user/HIJACK' >> ~/.bashrc
rm -f /home/user/HIJACK; echo planted'''
s, out = run(attack)
check("alice puts programs, Python modules, and a configuration file into her home directory", s == 200 and "planted" in out, out)
uid_before = sandbox_pod("user:alice", "personal")["metadata"]["uid"]
call("POST", "/api/v1/terminals/templates/personal/restart", user=None, body={"user_id": "alice", "chat_id": None, "model_id": None}, timeout=170)
check("the sandbox starts again with the same volume", wait(lambda: (sandbox_pod("user:alice", "personal") or {"metadata": {"uid": uid_before}})["metadata"]["uid"] != uid_before and sandbox_pod("user:alice", "personal")["status"].get("phase") == "Running", 120))
s, _ = call("GET", "/files/cwd")
check("the server of the sandbox answers with the key of warren after the restart", s == 200, s)
s, out = run("/bin/cat /home/user/HIJACK 2>&1; /bin/cat /proc/1/cmdline | /usr/bin/tr '\\0' ' '")
server_lines = [l for l in out.splitlines() if "open_terminal" in l and "sitecustomize" in l]
check("the server process did not load the Python module of the user", not server_lines, out)
check("the server is the program of the image", "/usr/bin/tini -- /usr/local/bin/python3 -I -u -m open_terminal run" in out, out)
call("DELETE", "/api/v1/terminals/templates/personal/instance", user=None, body={"user_id": "alice", "chat_id": None, "model_id": None}, timeout=170)

# ---------- warren repairs network policies ----------
s, out = run("echo again")
pa = sandbox_pod("user:alice", "personal"); IPA = pa["status"]["podIP"]; start = pa["metadata"]["labels"][P + "start"]
kubectl("-n", "warren-sandboxes", "delete", "networkpolicy", "warren-default-deny", start)
names = lambda: [x["metadata"]["name"] for x in items("networkpolicy")]
check("warren makes deleted network policies again", wait(lambda: "warren-default-deny" in names() and start in names(), 90), names())
check("after the repair, warren can connect to the sandbox", wait(lambda: call("GET", "/files/cwd")[0] == 200, 30))
check("after the repair, alice still cannot connect to bob", probe(IPB, 8000) == "timeout")
open_all = json.dumps({"spec": {"ingress": [{}], "egress": [{}]}})
kubectl("-n", "warren-sandboxes", "patch", "networkpolicy", "warren-default-deny", "--type=merge", "-p", open_all)
check("warren repairs the policy of the namespace", wait(lambda: json.loads(kubectl("-n", "warren-sandboxes", "get", "networkpolicy", "warren-default-deny", "-o", "json"))["spec"].get("egress") in (None, []), 90))
check("after the repair, alice cannot connect to bob again", wait(lambda: probe(IPB, 8000) == "timeout", 30))

# ---------- a change of the policy file applies to a running sandbox ----------
check("before the change, alice can connect to a public address", probe(PUBLIC_OK, 18080) == "open")
cm = json.loads(kubectl("-n", "warren-system", "get", "cm", "warren-policies", "-o", "json"))
text = cm["data"]["policies.yaml"]
old = "  - id: personal\n    name: Personal sandbox\n    scope: user\n"
assert text.count(old) == 1, text[:400]
kubectl("-n", "warren-system", "patch", "cm", "warren-policies", "--type=merge", "-p", json.dumps({"data": {"policies.yaml": text.replace(old, old + "    network: {egress: none}\n")}}))
print("INFO wait until Kubernetes and warren apply the new policy file (up to 4 minutes)", flush=True)
uid = sandbox_pod("user:alice", "personal")["metadata"]["uid"]
check("after the change to no network, warren stops the sandbox: an open connection must not continue", wait(lambda: (sandbox_pod("user:alice", "personal") or {"metadata": {"uid": ""}})["metadata"]["uid"] != uid, 260, 5))
check("the next sandbox of alice cannot connect to a public address", probe(PUBLIC_OK, 18080) == "timeout", probe(PUBLIC_OK, 18080))

log = kubectl("-n", "warren-system", "logs", "deploy/warren")
check("warren tested that the cluster applies network policies", "the cluster applies network policies" in log, [l[:160] for l in log.splitlines() if "cluster" in l][-3:])
check("no test pod stays in the sandbox namespace", not [p for p in items("pods") if p["metadata"]["name"].startswith("warren-canary")])
check("audit records for the network policies", "network policy made" in log and "network policy of a sandbox changed" in log, [l[:160] for l in log.splitlines() if "network policy" in l][-4:])
print("FAILED:", fails if fails else "none")
sys.exit(1 if fails else 0)
