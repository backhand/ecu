# ECU control plane on k3s

Manifests to run the ECU control plane in a k3s (or any standard Kubernetes)
cluster, wired together with **Kustomize**. TLS is terminated by the **Ingress**
(traefik, the k3s default), so the pod itself runs `ECU_TLS=off` and serves
plain HTTP — no privileged ports in-cluster.

Everything you customize per-deployment lives in **`kustomization.yaml`**: the
namespace, the control-plane image + tag, and the `ecu-settings` block — the
public hostname plus the Hetzner instance type/region for session VMs. Those are
set once there and stamped into the ConfigMap (and, for the hostname, the
Ingress) by replacements, so the copies can't drift apart.

## Files

| File                 | What it is                                                    |
| -------------------- | ------------------------------------------------------------- |
| `kustomization.yaml` | Ties it together; holds the customizable knobs (namespace, image tag, **hostname**, **instance type/region**). |
| `namespace.yaml`     | The `ecu` Namespace everything is placed in.                  |
| `configmap.yaml`     | `ecu` ConfigMap — non-secret env (`ECU_TLS`, `ECU_LISTEN`, `ECU_DB`, `ECU_HOSTNAME`, `ECU_INSTANCE_TYPE`, `ECU_REGION`). |
| `secret.yaml`        | `ecu-secrets` — API key, Hetzner token, watch signing key. **Gitignored; copy from `secret.yaml.example`.** |
| `deployment.yaml`    | `ecu-controlplane` Deployment (1 replica) + `ecu-data` PVC; env via `envFrom`. |
| `service.yaml`       | `ecu-controlplane` ClusterIP Service (port 80 → pod 8080).    |
| `ingress.yaml`       | `ecu-controlplane` Ingress; terminates TLS, routes `/` to the Service. |

## Apply

Run these from this directory (`deploy/k3s/`).

1. **Fill in the secret.** `secret.yaml` is gitignored (it holds real values);
   copy the committed template and edit it:

   ```sh
   cp secret.yaml.example secret.yaml
   # edit secret.yaml — strong values:
   #   ECU_API_KEY / ECU_SIGNING_KEY  ->  openssl rand -hex 32
   #   ECU_HCLOUD_TOKEN               ->  your Hetzner Cloud API token
   ```

   (Prefer to keep secrets out of files entirely? Drop `secret.yaml` from
   `kustomization.yaml`'s `resources:` and `kubectl create secret generic
   ecu-secrets --from-literal=...` out-of-band instead.)

2. **Set your deployment settings — one place.** In the `ecu-settings` block of
   `kustomization.yaml`, set `ECU_HOSTNAME` and the Hetzner `ECU_INSTANCE_TYPE` /
   `ECU_REGION` for session VMs. Kustomize stamps them into the ConfigMap (and
   the hostname into the Ingress TLS host + routing rule) for you. While you're
   there, optionally pin the image tag (`images:`) and change the namespace.

3. **Render / apply:**

   ```sh
   kubectl kustomize .     # preview the rendered manifests
   kubectl apply -k .      # create the namespace + everything in it
   ```

4. **Point DNS** — create an A record for your host at the Ingress's external
   IP / load balancer address.

## TLS

The Ingress presents the certificate. Pick one issuance path (see comments in
`ingress.yaml`):

- **cert-manager** — install it plus a `letsencrypt-prod` ClusterIssuer and
  uncomment the `cert-manager.io/cluster-issuer` annotation; it fills the
  `ecu-tls` Secret automatically.
- **traefik built-in ACME** — configure traefik's `certificatesResolvers` and
  annotate the Ingress with the resolver.

## Notes

- **Singleton.** The control plane owns an embedded SQLite DB and an in-memory
  tunnel registry, so it must run as **one** replica. Do not scale it; the
  rollout strategy is `Recreate` so two pods never run at once.
- **Persistence.** The `ecu-data` PVC (1Gi, RWO) holds the DB + seeded key
  state. An `emptyDir` works for throwaway testing but loses all state (admin
  key, sessions, persistence records) on restart.
- **WebSocket.** The agent tunnel (`/agent/connect`) and live watch
  (`/sessions/{id}/watch`) ride WebSocket; traefik proxies WS transparently, so
  no extra Ingress configuration is needed.
