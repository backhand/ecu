# ECU control plane on k3s

Manifests to run the ECU control plane in a k3s (or any standard Kubernetes)
cluster. TLS is terminated by the **Ingress** (traefik, the k3s default), so the
pod itself runs `ECU_TLS=off` and serves plain HTTP — no privileged ports
in-cluster.

## Files

| File              | What it is                                                    |
| ----------------- | ------------------------------------------------------------- |
| `secret.yaml`     | `ecu-secrets` — API key, Hetzner token, watch signing key. **Template; do not commit real values.** |
| `deployment.yaml` | `ecu-controlplane` Deployment (1 replica) + `ecu-data` PVC.    |
| `service.yaml`    | `ecu-controlplane` ClusterIP Service (port 80 → pod 8080).    |
| `ingress.yaml`    | `ecu-controlplane` Ingress; terminates TLS, routes `/` to the Service. |

## Apply

1. **Create the secret** (do NOT `kubectl apply` the template with real values):

   ```sh
   kubectl create secret generic ecu-secrets \
     --from-literal=ECU_API_KEY="$(openssl rand -hex 32)" \
     --from-literal=ECU_HCLOUD_TOKEN="<hetzner-cloud-api-token>" \
     --from-literal=ECU_SIGNING_KEY="$(openssl rand -hex 32)"
   ```

2. **Set your hostname.** Replace `ecu.example.com` in `deployment.yaml`
   (`ECU_HOSTNAME`) and `ingress.yaml` (the `tls` host and the rule `host`) with
   your real DNS name.

3. **Apply the rest:**

   ```sh
   kubectl apply -f deployment.yaml -f service.yaml -f ingress.yaml
   # or: kubectl apply -f .   (after creating the secret out-of-band)
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
