.PHONY: up down down-v migrate migrate-down migrate-status lint test test-short test-cover build tidy help \
        deploy deploy-api nginx-reload guard-clean-shell check-ports security-check

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

# ── Деплой-гигиена (инцидент 2026-07-03) ────────────────────────────────────
# Экспортированные в шелл переменные ПЕРЕБИВАЮТ значения из .env при
# интерполяции ${...} в compose (грабля «set -a; . ./.env» — ловили дважды:
# OZON_PROXY_URL и DATABASE_URL). Деплой только из чистого шелла.
guard-clean-shell:
	@bad=""; for v in DATABASE_URL REDIS_URL POSTGRES_PASSWORD REDIS_PASSWORD \
	                  APP_DB_PASSWORD MONITOR_DB_PASSWORD GRAFANA_ADMIN_PASSWORD \
	                  OZON_PROXY_URL TELEGRAM_BOT_TOKEN; do \
	    if printenv $$v >/dev/null 2>&1; then bad="$$bad $$v"; fi; \
	done; \
	if [ -n "$$bad" ]; then \
	    echo "⛔ в шелле экспортированы:$$bad"; \
	    echo "   Они перебьют \$${...} из .env в docker compose. Открой чистый шелл и повтори."; \
	    exit 1; \
	fi

# Наружу (0.0.0.0/[::]) должен быть опубликован только nginx 80/443.
check-ports:
	@open=$$(docker ps --format '{{.Names}}: {{.Ports}}' | tr ',' '\n' \
	    | grep -E '0\.0\.0\.0|\[::\]' | grep -vE ':(80|443)->' || true); \
	if [ -n "$$open" ]; then \
	    echo "!!! ОТКРЫТЫЕ НАРУЖУ ПОРТЫ (регресс инцидента 2026-07-03):"; \
	    echo "$$open"; exit 1; \
	else echo "ok: наружу только nginx 80/443"; fi

# Полный self-check (порты + redis + pg-роли + фаервол) — то же, что гоняет cron.
security-check:
	./scripts/security-selfcheck.sh

# Deploy (на VPS вручную, из каталога с .env и секретами)
# Любая пересборка api идёт с GIT_COMMIT → версионирование ассетов (?v=) корректное.
deploy-api: guard-clean-shell
	GIT_COMMIT=$(GIT_COMMIT) $(COMPOSE_PROD) build api
	$(COMPOSE_PROD) up -d api
	$(MAKE) check-ports

deploy: guard-clean-shell
	GIT_COMMIT=$(GIT_COMMIT) $(COMPOSE_PROD) build api bot-worker scraper search-worker reseller-worker notifier scheduler
	$(COMPOSE_PROD) up -d
	$(MAKE) nginx-reload
	$(MAKE) check-ports

nginx-reload:
	$(COMPOSE_PROD) exec -T nginx nginx -t
	$(COMPOSE_PROD) exec -T nginx nginx -s reload

# Help
help:
	@grep -E '^[a-z-]+:' $(MAKEFILE_LIST) | awk -F: '{print "  make " $$1}'