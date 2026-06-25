.PHONY: up down down-v migrate migrate-down migrate-status lint test test-short test-cover build tidy help \
        deploy deploy-api nginx-reload

DB_URL ?= postgres://user:password@localhost:5433/tryberrybot?sslmode=disable

# Prod-окружение на VPS: основной compose + overlay. GIT_COMMIT → ?v= у статики api.
COMPOSE_PROD = docker compose -f docker-compose.yml -f docker-compose.prod.yml
GIT_COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

# Docker
up:
	docker compose up -d
	@echo "✅ Services started"

down:
	docker compose down

down-v:
	docker compose down -v

logs:
	docker compose logs -f

# Migrations
migrate:
	@which goose > /dev/null || go install github.com/pressly/goose/v3/cmd/goose@latest
	goose -dir ./migrations postgres "$(DB_URL)" up

migrate-down:
	@which goose > /dev/null || go install github.com/pressly/goose/v3/cmd/goose@latest
	goose -dir ./migrations postgres "$(DB_URL)" down

migrate-status:
	@which goose > /dev/null || go install github.com/pressly/goose/v3/cmd/goose@latest
	goose -dir ./migrations postgres "$(DB_URL)" status

# Code quality
lint:
	@which golangci-lint > /dev/null || go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	golangci-lint run ./...

tidy:
	go mod tidy

# Tests
test:
	go test -race -count=1 ./...

test-short:
	go test -short -race -count=1 ./...

test-cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

# Build
build:
	go build -o bin/api           ./cmd/api
	go build -o bin/scraper       ./cmd/scraper
	go build -o bin/notifier      ./cmd/notifier
	go build -o bin/scheduler     ./cmd/scheduler
	go build -o bin/search-worker ./cmd/search-worker

# Deploy (на VPS вручную, из каталога с .env и секретами)
# Любая пересборка api идёт с GIT_COMMIT → версионирование ассетов (?v=) корректное.
deploy-api:
	GIT_COMMIT=$(GIT_COMMIT) $(COMPOSE_PROD) build api
	$(COMPOSE_PROD) up -d api

deploy:
	GIT_COMMIT=$(GIT_COMMIT) $(COMPOSE_PROD) build api bot-worker scraper search-worker reseller-worker notifier scheduler
	$(COMPOSE_PROD) up -d
	$(MAKE) nginx-reload

nginx-reload:
	$(COMPOSE_PROD) exec -T nginx nginx -t
	$(COMPOSE_PROD) exec -T nginx nginx -s reload

# Help
help:
	@grep -E '^[a-z-]+:' $(MAKEFILE_LIST) | awk -F: '{print "  make " $$1}'