# Kubernetes

CrashCart as a Deployment behind an Ingress, with Postgres either managed
or as a small in-cluster StatefulSet. Any Postgres 15+ does — see
[The database](./postgres).

**You need**

- A cluster with an ingress controller (the manifests assume
  `ingress-nginx`; adjust `ingressClassName` and the body-size annotation
  for others)
- A domain, e.g. `crashcart.example.com`, pointing at the ingress
- Optionally [cert-manager](https://cert-manager.io) for TLS — or terminate
  TLS at your load balancer

## 1. Save the manifests

`crashcart.yaml` — namespace, secret, config, deployment, service, ingress:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: crashcart
---
apiVersion: v1
kind: Secret
metadata:
  name: crashcart
  namespace: crashcart
type: Opaque
stringData:
  # Replace every value. Generate keys with: openssl rand -hex 32
  POSTGRES_PASSWORD: change-me
  DATABASE_URL: postgres://crashcart:change-me@postgres:5432/crashcart?sslmode=disable
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: crashcart
  namespace: crashcart
data:
  PUBLIC_URL: https://crashcart.example.com
  RETENTION_DAYS: "30"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: crashcart
  namespace: crashcart
spec:
  replicas: 1
  selector:
    matchLabels:
      app: crashcart
  template:
    metadata:
      labels:
        app: crashcart
    spec:
      containers:
        - name: crashcart
          image: ghcr.io/at-least/crashcart:latest
          ports:
            - containerPort: 8080
          envFrom:
            - configMapRef:
                name: crashcart
            - secretRef:
                name: crashcart
          readinessProbe:
            httpGet:
              path: /health
              port: 8080
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /health
              port: 8080
            periodSeconds: 30
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              memory: 512Mi
---
apiVersion: v1
kind: Service
metadata:
  name: crashcart
  namespace: crashcart
spec:
  selector:
    app: crashcart
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: crashcart
  namespace: crashcart
  annotations:
    # Crash reports can be large.
    nginx.ingress.kubernetes.io/proxy-body-size: 25m
    # Uncomment with cert-manager installed:
    # cert-manager.io/cluster-issuer: letsencrypt
spec:
  ingressClassName: nginx
  rules:
    - host: crashcart.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: crashcart
                port:
                  number: 80
  # tls:
  #   - hosts: [crashcart.example.com]
  #     secretName: crashcart-tls
```

If you're using a **managed Postgres**, set `DATABASE_URL` in the Secret
to its connection string and skip the next file.

`postgres.yaml` — an in-cluster Postgres for small installs:

```yaml
# In-cluster Postgres. Give it a StorageClass you trust; or use a managed
# Postgres and skip this file.
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: crashcart
spec:
  selector:
    app: postgres
  ports:
    - port: 5432
  clusterIP: None
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: crashcart
spec:
  serviceName: postgres
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          env:
            - name: POSTGRES_USER
              value: crashcart
            - name: POSTGRES_DB
              value: crashcart
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: crashcart
                  key: POSTGRES_PASSWORD
            - name: PGDATA
              value: /var/lib/postgresql/data/pgdata
          ports:
            - containerPort: 5432
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "crashcart", "-d", "crashcart"]
            periodSeconds: 5
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 20Gi
```

## 2. Edit the values

In `crashcart.yaml`:

- **Secret**: replace every `change-me`. Keep `POSTGRES_PASSWORD` and the
  password inside `DATABASE_URL` identical. Generate keys with
  `openssl rand -hex 32`.
- **ConfigMap**: `PUBLIC_URL` is the HTTPS address your apps will use.
- **Ingress**: your host name; uncomment the `tls` block and the
  cert-manager annotation if you use it.

## 3. Apply

```sh
kubectl apply -f crashcart.yaml -f postgres.yaml
kubectl -n crashcart rollout status deployment/crashcart
```

CrashCart may restart once or twice while Postgres is still starting —
that's Kubernetes doing its job. Once `rollout status` reports success:

```sh
kubectl -n crashcart get pods
# crashcart-…   1/1   Running
# postgres-0    1/1   Running
```

## 4. Check

```sh
curl https://crashcart.example.com/health
# {"status":"ok"}
```

Before DNS or TLS are in place, test through a port-forward:

```sh
kubectl -n crashcart port-forward svc/crashcart 8080:80
curl http://localhost:8080/health
```

## 5. Create a project

```sh
kubectl -n crashcart exec deploy/crashcart -- /crashcart project shop-ios "Shop app (iOS)" ios
# project shop-ios (id 1)
# DSN: https://<key>@crashcart.example.com/1
```

Open `https://crashcart.example.com`, create the first account on the
`/setup` page, paste the DSN into the SDK —
[Connect an SDK](/guide/sdks) — and go through
[Before going live](./checklist) once.

## Upgrading

Pin the image to a release and change it to upgrade:

```sh
kubectl -n crashcart set image deployment/crashcart crashcart=ghcr.io/at-least/crashcart:0.2.0
kubectl -n crashcart rollout status deployment/crashcart
```

The schema is created when the new pod starts. With `:latest`,
`kubectl -n crashcart rollout restart deployment/crashcart` pulls the
newest release. See [Operations → Upgrading](./operations#upgrading) for
what changes if a release bumps the schema.

## Scaling

CrashCart keeps no local state; raise `replicas` freely. All pods share
the one database, including background work. The one exception is
`BLOB_STORE=fs` — symbol files in a directory are local to the pod that
wrote them — so with more than one replica keep symbol files in the
database (the default) or in a bucket (`BLOB_STORE=s3`).

## Backups

```sh
kubectl -n crashcart exec deploy/crashcart -- /crashcart export > backup-$(date +%F).ndjson
```

Restore with `kubectl -n crashcart exec -i deploy/crashcart -- /crashcart import < backup.ndjson`.
For the in-cluster Postgres, also snapshot the `data-postgres-0`
PersistentVolumeClaim. See [Operations](./operations#backups).

## iOS crashes

Run the `container/symbolicate` image from the repository as its own
Deployment + Service in the namespace and set
`SYMBOLICATE_URL: http://symbolicate:8080` in the ConfigMap. Give it a
volume at `/var/cache/crashcart-symbols` if you want its dSYM cache to
survive restarts (it refills from the database otherwise). See
[Symbolication](/guide/symbolication#ios-macos) for what it does and why.

## If something doesn't work

| Symptom | Check |
|---|---|
| `crashcart` pod in `CrashLoopBackOff` for more than a minute | `kubectl -n crashcart logs deploy/crashcart` — usually `DATABASE_URL` is wrong or Postgres isn't reachable |
| `/health` returns `503` | Postgres is down: `kubectl -n crashcart logs postgres-0` |
| The DSN shows the wrong host | Fix `PUBLIC_URL` in the ConfigMap and `rollout restart` |
| Large crash reports fail with `413` | Raise the ingress body-size annotation |
| `postgres-0` stuck `Pending` | No default StorageClass; set `storageClassName` in the volume claim template |
