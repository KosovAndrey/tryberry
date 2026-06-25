# ── Builder ──────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Кэшируем зависимости отдельным слоем
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходники
COPY . .

# ARG указывает какой бинарь собирать — api, scraper или notifier
ARG SERVICE
# GIT_COMMIT — короткий хеш для версионирования статики (?v=) у api. Для прочих
# сервисов -X в несуществующий символ линкер молча игнорирует.
ARG GIT_COMMIT=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -X main.assetVersion=${GIT_COMMIT}" \
    -o /app/bin/service ./cmd/${SERVICE}

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.20

# CA-сертификаты нужны для HTTPS запросов (Telegram, WB API, Я.Маркет)
RUN apk add --no-cache ca-certificates tzdata

# Непривилегированный пользователь — секурити best practice
RUN addgroup -S app && adduser -S app -G app
USER app

WORKDIR /app
COPY --from=builder /app/bin/service /app/service

ENTRYPOINT ["/app/service"]