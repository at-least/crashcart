# Kubernetes

CrashCart as a Deployment behind an Ingress, with Postgres either managed
or as a small in-cluster StatefulSet, and an S3-compatible bucket.

::: info Storage
Any Postgres 14+ — managed, or the in-cluster `postgres.yaml` below. The
bucket is your cloud's (S3, GCS with the S3 API, R2, …) or a MinIO you
run in the cluster (`minio.yaml` below). See
[The database and the object store](./postgres).
:::

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
  # The bucket. For a cloud bucket, its endpoint / region / keys instead.
  S3_ENDPOINT: http://minio:9000
  S3_BUCKET: crashcart
  S3_ACCESS_KEY: crashcart
  S3_SECRET_KEY: change-me-too
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
          image: ghcr.io/crashcartapp/crashcart:latest
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
to its connection string and skip the next file; with a **cloud bucket**,
set the `S3_*` values to it and skip `minio.yaml`.

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

`minio.yaml` — an in-cluster object store for small installs:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: crashcart
spec:
  selector:
    app: minio
  ports:
    - port: 9000
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: minio
  namespace: crashcart
spec:
  serviceName: minio
  replicas: 1
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
        - name: minio
          image: minio/minio
          args: ["server", "/data"]
          env:
            - name: MINIO_ROOT_USER
              valueFrom:
                secretKeyRef:
                  name: crashcart
                  key: S3_ACCESS_KEY
            - name: MINIO_ROOT_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: crashcart
                  key: S3_SECRET_KEY
          ports:
            - containerPort: 9000
          volumeMounts:
            - name: data
              mountPath: /data
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 50Gi
```

## 2. Edit the values

In `crashcart.yaml`:

- **Secret**: replace every `change-me`. Keep `POSTGRES_PASSWORD` and the
  password inside `DATABASE_URL` identical; `S3_ACCESS_KEY` /
  `S3_SECRET_KEY` are MinIO's root credentials when you run `minio.yaml`.
  Generate keys with `openssl rand -hex 32`.
- **ConfigMap**: `PUBLIC_URL` is the HTTPS address your apps will use.
- **Ingress**: your host name; uncomment the `tls` block and the
  cert-manager annotation if you use it.

## 3. Apply

```sh
kubectl apply -f crashcart.yaml -f postgres.yaml -f minio.yaml
kubectl -n crashcart rollout status deployment/crashcart
```

CrashCart may restart once or twice while Postgres and MinIO are still
starting — that's Kubernetes doing its job. Once `rollout status` reports
success:

```sh
kubectl -n crashcart get pods
# crashcart-…   1/1   Running
# postgres-0    1/1   Running
# minio-0       1/1   Running
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
kubectl -n crashcart set image deployment/crashcart crashcart=ghcr.io/crashcartapp/crashcart:0.2.0
kubectl -n crashcart rollout status deployment/crashcart
```

The schema is created when the new pod starts. With `:latest`,
`kubectl -n crashcart rollout restart deployment/crashcart` pulls the
newest release.

## Scaling

CrashCart keeps no local state; raise `replicas` freely. All pods share
the one database, including background work.

## Backups

```sh
kubectl -n crashcart exec deploy/crashcart -- /crashcart export > backup-$(date +%F).ndjson
```

Restore with `kubectl -n crashcart exec -i deploy/crashcart -- /crashcart import < backup.ndjson`.
For the in-cluster Postgres and MinIO, also snapshot the `data-postgres-0`
and `data-minio-0` PersistentVolumeClaims. See [Operations](./operations#backups).

## iOS crashes

Run the `container/symbolicate` image from the repository as its own
Deployment + Service in the namespace and set
`SYMBOLICATE_URL: http://symbolicate:8080` in the ConfigMap. Android and
JavaScript need nothing extra.

## If something doesn't work

| Symptom | Check |
|---|---|
| `crashcart` pod in `CrashLoopBackOff` for more than a minute | `kubectl -n crashcart logs deploy/crashcart` — usually `DATABASE_URL` is wrong or Postgres isn't reachable |
| `/health` returns `503` | Postgres is down: `kubectl -n crashcart logs postgres-0` |
| The log says `payload pack` errors | MinIO (or the bucket) is unreachable or the keys are wrong: `kubectl -n crashcart logs minio-0`. Nothing is lost: payloads wait in Postgres until the bucket is back |
| The DSN shows the wrong host | Fix `PUBLIC_URL` in the ConfigMap and `rollout restart` |
| Large crash reports fail with `413` | Raise the ingress body-size annotation |
| `postgres-0` or `minio-0` stuck `Pending` | No default StorageClass; set `storageClassName` in the volume claim template |
