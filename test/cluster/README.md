# Cluster checks

These scripts test warren on a real Kubernetes cluster. They use a kind cluster on your computer, the published Open Terminal images, and a build of warren from this repository.

The checks are manual. The CI of this repository does not run them. They call warren directly and do not use Open WebUI.

## 1. What the checks cover

| Script | Subject |
|---|---|
| `lifecycle.py` | The start, the stop, the idle time, and the limits of a sandbox. The limit variables of the pod. The system information of a sandbox that operates and of a stopped sandbox. The close code of an open WebSocket at a stop operation. A watcher WebSocket of a stopped sandbox. An open WebSocket that sends only keepalive messages, at the idle stop. The authentication of each request. |
| `storage.py` | Volumes, the policy file, the instance operations, the retention time, and the user list of a policy. |
| `isolation.py` | The network rules of a sandbox, the security settings of the pod, and the separation between two users. |

Each script prints one line for each check. A line starts with `PASS` or `FAIL`. The last line gives the list of the checks that failed.

## 2. Requirements

- Docker
- kind 0.33 or later
- Python 3.10 or later
- The Python package `websockets`, for `lifecycle.py`
- A network connection to `ghcr.io`, to `raw.githubusercontent.com`, and to the registry of the Calico images

## 3. Make the cluster

```bash
test/cluster/setup.sh
```

The script makes a kind cluster with the name `warren-dev` and installs Calico. Calico is necessary because the default network plugin of kind does not enforce NetworkPolicy on all kernels. If a cluster does not enforce NetworkPolicy, warren does not become ready.

If your network cannot connect to `quay.io`, use the Calico images from Docker Hub:

```bash
CALICO_REGISTRY=docker.io test/cluster/setup.sh
```

## 4. Do the checks

```bash
test/cluster/run.sh
```

The script does these steps:

1. It builds the warren image from the repository.
2. It copies the warren image and three Open Terminal images into the cluster.
3. For each check, it removes all sandboxes, applies `manifests.yaml`, and starts warren again.
4. It writes the output of each check to a file with the extension `.out` in this directory.

To do only some checks, give their names:

```bash
test/cluster/run.sh isolation storage
```

The three checks together take approximately 30 minutes. Most of that time is waiting for an idle time or a retention time to end.

To use a warren image that Docker already has, give its name. The script then does not build an image:

```bash
WARREN_IMAGE=warren:local test/cluster/run.sh
```

If Python is in a virtual environment, give its path:

```bash
PYTHON=/path/to/venv/bin/python test/cluster/run.sh
```

## 5. Remove the cluster

```bash
kind delete cluster --name warren-dev
```

## 6. Limits

- The cluster has one node. The checks do not cover a volume or a network rule across two nodes.
- The default storage class of kind does not enforce the size of a volume.
- The manifests in `manifests.yaml` are only for these checks. They use a fixed connection key and short times. Do not use them for a real installation.
