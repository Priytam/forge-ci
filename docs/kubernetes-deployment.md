# Kubernetes executor: deployment, access & RBAC

What exactly the kubernetes executor needs — ServiceAccount permissions, pod
requirements, and network paths — for both topologies: manager inside the
job cluster, and manager targeting a **separate** cluster.

## Who talks to whom

| Path | Protocol | Needed by |
|---|---|---|
| manager → forge-server | HTTP(S) outbound (`SERVER_URL`) | acquire jobs, stream logs, upload/download artifacts |
| manager → VCS (github.com / bitbucket.org) | HTTPS outbound | source clone happens **on the manager** |
| manager → job cluster API server | HTTPS (kubectl) | pod create/exec/cp/delete |
| job pod → image registry | HTTPS | image pull (kubelet) |
| job pod → anything else | — | **nothing required** — job pods never talk to forge-server or the VCS; source and artifacts move through the manager via `kubectl cp` |

That last row is what makes the separate-cluster case easy: the job cluster
needs **no route to Forge at all** — only the manager needs connectivity to
both sides.

## Exact RBAC (namespace-scoped)

The executor issues: `kubectl run` (create pod), `kubectl wait` (get/watch),
`kubectl cp` and `kubectl exec` (pods/exec), `kubectl delete pod`. Nothing
cluster-scoped, nothing beyond one namespace:

```yaml
apiVersion: v1
kind: Namespace
metadata: {name: forge-ci}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: forge-runner, namespace: forge-ci}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: forge-runner, namespace: forge-ci}
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["create", "get", "list", "watch", "delete"]
- apiGroups: [""]
  resources: ["pods/exec"]        # kubectl exec AND kubectl cp
  verbs: ["create"]
- apiGroups: [""]
  resources: ["pods/log"]         # kubectl wait error diagnostics
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: forge-runner, namespace: forge-ci}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: forge-runner}
subjects:
- {kind: ServiceAccount, name: forge-runner, namespace: forge-ci}
```

## Topology A — manager inside the job cluster

1. Apply the RBAC above.
2. Build the manager image (includes kubectl + git):
   `docker build -f deploy/Dockerfile.runner-k8s -t <registry>/forge-runner-k8s .`
3. Deploy the manager with `KUBE_CONTEXT=in-cluster` (uses the pod's
   ServiceAccount — no kubeconfig needed):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: forge-runner-manager, namespace: forge-ci}
spec:
  replicas: 1                    # one manager; scale jobs with RUNNER_CONCURRENCY
  selector: {matchLabels: {app: forge-runner-manager}}
  template:
    metadata: {labels: {app: forge-runner-manager}}
    spec:
      serviceAccountName: forge-runner
      containers:
      - name: manager
        image: <registry>/forge-runner-k8s:latest
        env:
        - {name: SERVER_URL, value: "https://forge.internal.example.com"}
        - {name: EXECUTOR, value: "kubernetes"}
        - {name: KUBE_CONTEXT, value: "in-cluster"}
        - {name: KUBE_NAMESPACE, value: "forge-ci"}
        - {name: RUNNER_TAGS, value: "k8s"}
        - {name: RUNNER_CONCURRENCY, value: "4"}
        - name: RUNNER_ID
          valueFrom: {fieldRef: {fieldPath: metadata.name}}
```

## Topology B — manager targets a separate cluster

The manager runs anywhere (a VM next to forge-server, another cluster) and
drives job pods in a remote cluster it holds credentials for.

1. In the **job cluster**: apply the RBAC above, then mint a token for the
   ServiceAccount (long-lived secret form, k8s ≥1.24):
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: forge-runner-token
     namespace: forge-ci
     annotations: {kubernetes.io/service-account.name: forge-runner}
   type: kubernetes.io/service-account-token
   ```
   ```sh
   TOKEN=$(kubectl -n forge-ci get secret forge-runner-token -o jsonpath='{.data.token}' | base64 -d)
   CA=$(kubectl -n forge-ci get secret forge-runner-token -o jsonpath='{.data.ca\.crt}')
   APISERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
   ```
2. Build a dedicated kubeconfig for the manager (never reuse a human's):
   ```sh
   cat > forge-runner.kubeconfig <<EOF
   apiVersion: v1
   kind: Config
   clusters:
   - name: ci-cluster
     cluster: {server: $APISERVER, certificate-authority-data: $CA}
   users:
   - name: forge-runner
     user: {token: $TOKEN}
   contexts:
   - name: forge-ci
     context: {cluster: ci-cluster, user: forge-runner, namespace: forge-ci}
   EOF
   ```
3. Run the manager pointing at that context (and only that file):
   ```sh
   KUBECONFIG=/etc/forge/forge-runner.kubeconfig \
   KUBE_CONTEXT=forge-ci KUBE_NAMESPACE=forge-ci \
   SERVER_URL=https://forge.internal.example.com \
   ./forge-runner --executor=kubernetes --id=k8s-remote-1 --tags=k8s --concurrency=4
   ```
   If the manager itself runs in some cluster, mount the kubeconfig from a
   Secret and set `KUBECONFIG` to the mount path in the Deployment.
4. Network: the manager host must reach the remote API server (firewall /
   private endpoint allowlist). Job pods still need nothing.

## Job pod requirements & namespace hygiene

- **Images**: must contain `sh` and `tar` (alpine, debian, and typical build
  images qualify; distroless does not).
- **Registry access**: private registries need an imagePullSecret on the
  `forge-runner` ServiceAccount or namespace default.
- **Quotas**: job pods currently run without resource requests/limits — set a
  namespace `LimitRange` so runaway jobs can't starve the cluster:
  ```yaml
  apiVersion: v1
  kind: LimitRange
  metadata: {name: forge-job-defaults, namespace: forge-ci}
  spec:
    limits:
    - type: Container
      default: {cpu: "1", memory: 2Gi}
      defaultRequest: {cpu: 250m, memory: 256Mi}
  ```
- **Cleanup guarantees**: pods are deleted after each job; if a manager dies
  mid-job the pod's `sleep 7200` entrypoint self-terminates within 2h. All
  job pods carry `app=forge-ci-job` — a periodic
  `kubectl -n forge-ci delete pod -l app=forge-ci-job --field-selector=status.phase=Succeeded`
  is a belt-and-braces cron.
- **Isolation**: use a dedicated namespace per trust boundary; nothing in the
  executor requires privileged pods, hostPath, or cluster-scoped access.
