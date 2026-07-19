# Docker Tracker

A small, self-hosted Go application for finding and bulk-deleting Docker Hub tags. The frontend is embedded in the binary, and Docker Hub credentials are stored in an in-cluster Kubernetes Secret rather than an application database.

## Features

- Browse and filter Docker Hub repositories and tags
- Select tags across multiple repositories and delete up to 500 at a time
- Explicit `DELETE` confirmation and per-tag results
- Responsive light/dark interface
- Kubernetes-native Secret storage with narrowly scoped RBAC
- MetalLB-ready `LoadBalancer` Service
- Stateless deployment; no PersistentVolume or StorageClass is required
- Optional HTTP Basic authentication via `APP_USERNAME` and `APP_PASSWORD`

## Build and deploy

```sh
docker build -t harbor.mlinarik.com/mlinarik/docker-hub-manager:v0.1.1 .
docker push harbor.mlinarik.com/mlinarik/docker-hub-manager:v0.1.1
```

The deployment manifest uses that Harbor image. Apply it with:

```sh
kubectl apply -f deploy/kubernetes.yaml
kubectl -n docker-tracker get service docker-tracker
```

MetalLB assigns an address from its current pool. To request a specific address, uncomment and edit the `metallb.io/loadBalancerIPs` annotation. No PVC is included because all durable state is one Kubernetes Secret; consequently the cluster's StorageClass is intentionally untouched.

Open the service address and enter a Docker ID, personal access token, and optional organization namespace. The PAT needs repository Read, Write, and Delete permissions.

## Protect the UI

This app can delete registry data and should not be exposed anonymously. Place it behind an authenticated ingress or set `APP_USERNAME` and `APP_PASSWORD`. For example, create a separate Secret and add `secretKeyRef` environment variables to the Deployment.

## Local development

The app reads the Kubernetes API through the pod's service account, so normal use is in-cluster. For UI development, deploy it and port-forward:

```sh
kubectl -n docker-tracker port-forward service/docker-tracker 8080:80
```

Then open `http://localhost:8080`.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SECRET_NAME` | `docker-tracker-credentials` | Credential Secret name |
| `POD_NAMESPACE` | service-account namespace | Kubernetes namespace fallback |
| `DOCKER_HUB_URL` | `https://hub.docker.com` | Docker Hub API base (also useful for tests) |
| `APP_USERNAME` / `APP_PASSWORD` | unset | Enable HTTP Basic authentication |
