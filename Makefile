.PHONY: up down down-v migrate migrate-down migrate-status lint test test-short test-cover build tidy help

DB_URL ?= postgres://user:password@localhost:5433/tryberrybot?sslmode=disable

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
	go build -o bin/api      ./cmd/api
	go build -o bin/scraper  ./cmd/scraper
	go build -o bin/notifier ./cmd/notifier

# Help
help:
	@grep -E '^[a-z-]+:' $(MAKEFILE_LIST) | awk -F: '{print "  make " $$1}'