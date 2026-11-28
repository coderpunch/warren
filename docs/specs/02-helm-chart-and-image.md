# Specification 02: Helm chart and release of the container image

Status: proposal. The owner of the project must approve it.

## 1. Purpose

Warren has no published image and no chart. The only manifests are in `test/cluster/manifests.yaml`, and they are for a test.
This work lets an administrator install a release of warren with one command.

## 2. Scope

In scope:

- A Helm chart in the repository.
- A release workflow that publishes the container image and the chart.
- A version flag in the binary.
- An installation document.

Not in scope:

- A subchart of the Open WebUI chart (`feature-parity.md`, section 5: "Won't do").
- More than one replica.
- An operator or a custom resource.

## 3. Names

Do not use "Open WebUI" or "Terminals" in the name of the chart, the image, or a Kubernetes object (`CONTRIBUTING.md`, section 2).

| Item | Name |
|---|---|
| Chart | `warren` |
| Chart directory | `deploy/helm/warren` |
| Image | `ghcr.io/coderpunch/warren` |
| Chart registry | `oci://ghcr.io/coderpunch/charts/warren` |

## 4. Chart

### 4.1 Objects

| Object | Namespace | Notes |
|---|---|---|
| ServiceAccount | Release | `automountServiceAccountToken: true`. Warren needs the token. |
| Role and RoleBinding `warren` | Sandbox | The permissions of `policies.md`, section 12, and `isolation.md`, section 9. |
| Role and RoleBinding `warren-self` | Release | `get` on `pods`, so that warren can examine its own pod (`isolation.md`, section 5). |
| Namespace | Sandbox | Optional. Refer to section 4.3. |
| ConfigMap | Release | The policy file. Refer to section 4.4. |
| Secret | Release | The connection key. Refer to section 4.5. |
| Deployment | Release | Refer to section 4.6. |
| Service | Release | Type `ClusterIP`. It has only the port for Open WebUI traffic (8080). |
| NetworkPolicy | Release | Refer to section 4.7. |
| Service and ServiceMonitor for the metrics | Release | Optional. The default is "off". |

The permissions in the Role must be the same as the list `requiredPermissions` in `internal/kube/network.go`.
Warren stops at its start if a permission is absent. Thus the cluster checks of section 7 prove the Role.

### 4.2 Values

| Value | Default | Function |
|---|---|---|
| `image.repository`, `image.tag`, `image.digest` | The image of the chart version | A digest has priority over a tag. |
| `imagePullSecrets` | `[]` | For the warren pod. |
| `sandboxNamespace.name` | `warren-sandboxes` | The flag `--sandbox-namespace`. |
| `sandboxNamespace.create` | `true` | Refer to section 4.3. |
| `sandboxImage` | Empty | The flag `--sandbox-image`. Empty means the default of the binary. |
| `policies` | One policy: `personal`, scope `user` | The content of the policy file, as a YAML object. |
| `existingPolicyConfigMap` | Empty | Use a ConfigMap that the chart does not make. The key is `policies.yaml`. |
| `connectionKey.existingSecret.name`, `.key` | Empty, `key` | The Secret with the connection key. |
| `connectionKey.value` | Empty | The chart makes the Secret from this value. Only for a test. |
| `tls.existingSecret` | Empty | A Secret of the type `kubernetes.io/tls`. The chart sets the two TLS flags. |
| `openWebUI.namespaceSelector`, `openWebUI.podSelector` | `{}` | The pods that can connect to warren. Refer to section 4.7. |
| `networkPolicy.enabled` | `true` | Refer to section 4.7. |
| `limits.idleTimeout`, `.startTimeout`, `.maxSandboxesPerUser`, `.maxSandboxes`, `.maxVolumesPerUser`, `.maxVolumes`, `.volumeDeleteDelay` | Empty | Each value sets one flag. Empty means the default of the binary. |
| `network.egressBlockedCIDRs`, `.dnsNamespace`, `.dnsPodLabels`, `.dnsPorts`, `.dnsCIDRs`, `.acceptedNetworkPolicies` | Empty | The flags of `isolation.md`, section 6. |
| `logLevel` | `info` | |
| `extraArgs`, `extraEnv` | `[]` | For a flag that has no value in the chart. |
| `resources` | Requests: `100m`, `128Mi`. Limit: `512Mi` memory. | For the warren pod. |
| `nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, `podAnnotations` | Empty | For the warren pod. |
| `metrics.service.enabled`, `metrics.serviceMonitor.enabled`, `metrics.allowFrom` | `false`, `false`, `[]` | Refer to section 4.7. |

The chart does not have values for `--allow-other-pods`, `--skip-network-policy-test`, and `--allow-set-cookie`.
The documents do not recommend these flags. An administrator who needs one uses `extraArgs`.

The chart has a file `values.schema.json`. Helm refuses a value with a wrong type or an unknown name.

### 4.3 Sandbox namespace

When `sandboxNamespace.create` is true, the chart makes the namespace with these properties:

- The labels `pod-security.kubernetes.io/enforce: restricted` and `pod-security.kubernetes.io/enforce-version: latest`.
- The annotation `helm.sh/resource-policy: keep`. Helm then does not delete the namespace when a person removes the release. The namespace has the volumes of the users.

The chart fails when the sandbox namespace is the release namespace. Warren does not start in that condition (`isolation.md`, section 5).

### 4.4 Policy file

- The chart writes `policies` to the key `policies.yaml` of a ConfigMap.
- The pod has no checksum annotation for this ConfigMap. Warren reads a changed file with no restart (`policies.md`, section 11). A restart would close all open terminals.
- Helm cannot examine the file. The installation document tells the administrator to read the log of warren after a change.

### 4.5 Connection key

- The chart fails with a clear message when `connectionKey.existingSecret.name` and `connectionKey.value` are empty.
- The chart makes no random key. A key from the Helm function `lookup` changes with `helm template` and with tools that use it. A changed key breaks the connection to Open WebUI.
- The chart fails when `connectionKey.value` has fewer than 32 characters.
- The pod has no checksum annotation for the Secret. Warren reads a changed key file with no restart.

The installation document gives the command that makes a key:

```bash
kubectl -n warren-system create secret generic warren-key --from-literal=key="$(openssl rand -hex 32)"
```

### 4.6 Deployment

| Setting | Value | Cause |
|---|---|---|
| `replicas` | 1. The chart has no value for it. | Warren operates with one active replica (`feature-parity.md`, section 16). |
| `strategy` | `RollingUpdate`, `maxSurge: 1`, `maxUnavailable: 0` | The old pod serves until the new pod is ready. Two warren processes can operate for a short time (`isolation.md`, section 4). |
| `terminationGracePeriodSeconds` | 30 | The shutdown delay (5 seconds) and the time to close the WebSockets (15 seconds). |
| Pod labels | `app.kubernetes.io/name: warren`, `app.kubernetes.io/instance: <release>` | |
| Flag `--warren-pod-labels` | The two pod labels | The network policy of a sandbox selects the warren pods with them. |
| Variable `WARREN_POD_NAME` | From the field `metadata.name` | `isolation.md`, section 5. |
| Readiness probe | `GET /readyz` on port 9090 | A new pod that cannot prove the network isolation does not replace the old pod. |
| Liveness probe | `GET /healthz` on port 9090 | |
| Security context | Not root (user 65532), no privilege escalation, all capabilities dropped, read-only root file system, seccomp profile `RuntimeDefault` | The image has no shell and writes no files. |
| Volumes | The key Secret, the policy ConfigMap, and the TLS Secret, all read-only | |

The chart sets each flag with a `WARREN_` variable, and not with an argument. The variables are in `cmd/warren/main.go`, `parseFlags`.

### 4.7 Network policy of warren

Warren trusts each client that has the connection key (`threat-model.md`, section 3, assumption 1). Thus the chart limits the connections to warren.

When `networkPolicy.enabled` is true, the chart makes one NetworkPolicy that selects the warren pod:

- Ingress to port 8080: only from the pods of `openWebUI.podSelector` in the namespaces of `openWebUI.namespaceSelector`.
- Ingress to port 9090: only from the sources of `metrics.allowFrom`. The probes of the kubelet come from the node, and the usual network plugins permit them.
- No egress rule. A standard NetworkPolicy cannot name the Kubernetes API in a way that is correct on each cluster.

The chart fails when `networkPolicy.enabled` is true and the two `openWebUI` selectors are empty. An empty selector would permit all pods.

### 4.8 Notes after the installation

The file `NOTES.txt` tells the administrator:

1. The URL of warren for the connection in Open WebUI.
2. That the connection needs the key of the Secret and the authentication type `bearer`.
3. The command that shows the readiness of warren and its cause.

## 5. Version of the binary

- Add the variable `version` to the package `main`. The build sets it with `-ldflags "-X main.version=<version>"`. The default is `dev`.
- Add the flag `--version`. It prints the version and stops with the status 0.
- Add the metric `warren_build_info{version}` with the value 1.
- Write the version in the log record `listen`.
- The `Dockerfile` gets the build argument `VERSION`.

## 6. Release workflow

File: `.github/workflows/release.yml`. It starts on a tag of the form `v*.*.*`.

| Step | Detail |
|---|---|
| Checks | The jobs of `ci.yml`. The nightly end-to-end run of specification 01 must be successful for the same commit. |
| Image | Build for `linux/amd64` and `linux/arm64`. Push `ghcr.io/coderpunch/warren:<version>`. Do not push the tag `latest`. |
| Attestation | Make a build provenance attestation and an SBOM for the image digest. |
| Chart | Set `version` and `appVersion` to the version of the tag, and the default image digest to the digest of the new image. Push the chart to the chart registry. |
| GitHub release | Make a release with the image digest and the chart reference in its text. |

Rules:

- The workflow permissions are `contents: write` (for the release), `packages: write`, `id-token: write`, and `attestations: write`. All other permissions are off.
- Each action has a full commit ID, as in `ci.yml`.
- The workflow does not run for a pull request. It never uses the code of a fork.

## 7. Tests

| Test | Where | What it shows |
|---|---|---|
| `helm lint` | `ci.yml` | The chart has no syntax error. |
| `helm template` with the default values and with a file that sets each value, then a schema check of the result | `ci.yml` | Each object is a valid Kubernetes object. |
| Render tests | `ci.yml` | The chart fails for each condition of sections 4.3, 4.5, and 4.7. |
| Cluster checks | `test/cluster/run.sh` | The script installs warren with the chart and the file `test/cluster/values.yaml`. The file `manifests.yaml` goes away. All checks pass. |
| Upgrade check | `test/cluster/` | Install the chart, open a terminal, run `helm upgrade` with a new policy. The terminal stays open, and the new policy is in the template list. |

Add `deploy/` to `.dockerignore`. The image does not need the chart.

## 8. Documentation

- New document `docs/install.md`: requirements, the installation steps, the connection in Open WebUI, the upgrade, and the removal. It refers to the checklist of `threat-model.md`, section 6.
- `README.md`: add the installation document to the table. Remove "the Helm chart" from the list of work that is not complete.
- `policies.md`, section 12, and `isolation.md`, section 9: say that the chart makes the Role.
- `feature-parity.md`, section 5: the row about the subchart stays "Won't do". Add that warren has its own chart.

## 9. Open items

| No. | Item |
|---|---|
| O1 | Decision of the owner: the registry names of section 3. |
| O2 | Decision of the owner: a signature of the image (cosign) in the first release, or later. |
| O3 | Decision of the owner: the chart refuses an installation with no network policy selectors (section 4.7). A less strict default is possible, with a warning in `NOTES.txt`. |

## 10. Acceptance

1. On a new kind cluster, `helm install` with a key Secret and the Open WebUI selectors gives a warren that is ready.
2. All cluster checks pass with the chart.
3. A tag makes an image for two architectures, a chart, and a release.
4. `helm uninstall` keeps the sandbox namespace and its volumes.
5. `warren --version` prints the version of the tag.
