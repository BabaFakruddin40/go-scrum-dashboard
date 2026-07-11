.PHONY: help build up down restart logs

help:
	@echo "Available commands:"
	@echo "  make build   - Build the Docker images"
	@echo "  make rebuild - Rebuild the Docker images without cache"
	@echo "  make up      - Start app, postgres, and monitoring stack"
	@echo "  make down    - Stop the infrastructure"
	@echo "  make restart - Restart all components"
	@echo "  make logs    - Tail app logs"

build:
	docker-compose build

rebuild:
	docker-compose build --no-cache

up:
	docker-compose up -d
	@echo "🚀 Stack is running!"
	@echo "Go Dashboard: http://localhost:8080"
	@echo "Postgres:     localhost:5432 (scrum/scrum, db: scrum_dashboard)"
	@echo "Prometheus:   http://localhost:9090"
	@echo "Grafana:      http://localhost:3000 (User/Pass: admin/admin)"

down:
	docker-compose down

restart:
	docker-compose down && docker-compose up -d

logs:
	docker-compose logs -f app

helm:
	helm repo add aws-ebs-csi-driver https://kubernetes-sigs.github.io/aws-ebs-csi-driver
	helm repo update
	helm install aws-ebs-csi-driver aws-ebs-csi-driver/aws-ebs-csi-driver --namespace kube-system