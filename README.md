# Go Scrum Dashboard

A production-oriented Agile ticket dashboard built with Go, PostgreSQL, Prometheus, and Grafana.

This repo supports:
- Local development with Docker and Docker Compose
- Kubernetes deployment manifests
- AWS EKS + ECR infrastructure provisioning with Pulumi
- Application and cluster observability dashboards

## Features

- Ticket lifecycle workflow:
  - Open -> In Progress -> Code Review -> Test -> Verified -> Closed
- Enforced adjacent transitions (next/previous state only)
- Ticket CRUD:
  - Create, edit, delete
- Filtering:
  - By assignee and ticket type
- Persistent storage in PostgreSQL
- Prometheus metrics from the Go app:
  - `scrum_story_points_completed_total`
  - `scrum_active_bugs_count`
  - `scrum_sprint_velocity`

## Repository Structure

- `main.go` - Go web app + HTTP handlers + Prometheus instrumentation
- `Dockerfile` - App image build
- `compose.yml` - Local stack (app, postgres, prometheus, grafana)
- `prometheus.yml` - Local Prometheus scrape configuration
- `grafana/dashboards/` - Dashboard JSON files
- `k8s_manifest_configs/` - Kubernetes manifests for app and observability stack
- `scrum-infra-pulumi/` - Pulumi IaC for EKS, VPC, and ECR
- `Makefile` - Common local commands

## Prerequisites

- Docker + Docker Compose
- (Optional local) Go toolchain
- kubectl configured for your cluster
- AWS CLI configured (for EKS/ECR flow)
- Pulumi CLI + Node.js (for `scrum-infra-pulumi`)

## Local Deployment (Docker)

### 1. Build and run

```bash
make build
make up
```

Or rebuild without cache:

```bash
make rebuild
make up
```

### 2. Access services

- App: http://localhost:8080
- Prometheus: http://localhost:9090
- Grafana: http://localhost:3000
  - Username: `admin`
  - Password: `admin`
- PostgreSQL:
  - Host: `localhost`
  - Port: `5432`
  - DB: `scrum_dashboard`
  - User: `scrum`
  - Password: `scrum`

### 3. Stop

```bash
make down
```

## Kubernetes Deployment (Manifests)

Apply manifests from `k8s_manifest_configs`:

```bash
kubectl apply -f k8s_manifest_configs/postgres_deploy.yaml
kubectl apply -f k8s_manifest_configs/jboard_deploy.yaml
kubectl apply -f k8s_manifest_configs/prometheus_deploy.yaml
kubectl apply -f k8s_manifest_configs/grafana_deploy.yaml
```

Check pods/services:

```bash
kubectl get pods
kubectl get svc
```

Port-forward for local access:

```bash
kubectl port-forward svc/jboard 8000:8080
kubectl port-forward svc/prometheus 9090:9090
kubectl port-forward svc/grafana 3000:3000
```

## AWS EKS + ECR with Pulumi

Pulumi project lives in `scrum-infra-pulumi` and provisions:
- ECR repository for app images
- VPC + subnets + routing
- EKS cluster and node group

### 1. Deploy infrastructure

```bash
cd scrum-infra-pulumi
npm install
pulumi stack select <your-stack> || pulumi stack init <your-stack>
pulumi up
```

Capture outputs (ECR URL, kubeconfig, cluster name):

```bash
pulumi stack output
```

### 2. Build and push platform image to ECR

Use Docker Buildx for explicit platform image builds:

```bash
aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin <ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com

docker buildx build \
  --platform linux/amd64 \
  -t <ECR_REPO_URL>:v1 \
  --push \
  .
```

Update app image in `k8s_manifest_configs/jboard_deploy.yaml` and re-apply:

```bash
kubectl apply -f k8s_manifest_configs/jboard_deploy.yaml
```

## Observability

### Prometheus

- Local config: `prometheus.yml`
- K8s config: embedded in `k8s_manifest_configs/prometheus_deploy.yaml`
- Scrapes:
  - App metrics (`scrum-dashboard` job)
  - Kubernetes API server metrics
  - kube-state-metrics (cluster inventory metrics)

### Grafana Dashboards

Dashboard JSON files:
- `grafana/dashboards/jboard-k8s-dashboard.json`
- `grafana/dashboards/k8s-cluster-metrics-dashboard.json`

Import in Grafana:
- Dashboards -> Import -> Upload JSON
- Select datasource: `Prometheus`

## Troubleshooting

### Dashboard shows no data

1. Verify Prometheus targets are UP (`/targets`)
2. Verify metrics exist in Prometheus query UI (`/graph`), for example:

```promql
scrum_sprint_velocity
kube_pod_info
kube_service_info
kube_deployment_created
```

3. Ensure Grafana datasource points to `http://prometheus:9090` in cluster
4. Re-import dashboard JSON after updates

### Port-forward connection refused

Use correct service and target ports:

```bash
kubectl port-forward svc/jboard 8000:8080
```

## Makefile Shortcuts

```bash
make help
make build
make rebuild
make up
make down
make restart
make logs
```

## License

Internal/Project use.
