# ── Builder ──────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Кэшируем зависимости отдельным слоем
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Копируем исходники
COPY . .

# ARG указывает какой бинарь собирать — api, scraper или notifier
ARG SERVICE
# GIT_COMMIT — короткий хеш для версионирования статики (?v=) у api. Для прочих
# сервисов -X в несуществующий символ линкер молча игнорирует.
ARG GIT_COMMIT=dev
#
# BuildKit-кэш компиляции. Без него каждая сборка компилировала дерево
# зависимостей с нуля: `COPY . .` инвалидирует слой, а GOCACHE внутри контейнера
# живёт только внутри слоя и умирает вместе с ним. Замер на проде 2026-08-14:
# ОДИН деплой = 717 секунд, потому что семь сервисов собираются параллельно и
# каждый компилирует одни и те же otel/pgx/kafka-go отдельно.
#
# Кэш общий на все семь сборок (id по умолчанию = target), sharing=shared —
# обе кэш-директории Go рассчитаны на конкурентный доступ, блокировки Go берёт
# сам. Serialize через sharing=locked съел бы весь выигрыш от параллельности.
#
# Директиву `# syntax=docker/dockerfile:1` намеренно НЕ ставим: встроенный
# фронтенд BuildKit в Docker 23+ понимает cache-mount без неё, а директива
# добавила бы сетевую зависимость на образ фронтенда в единственном рабочем
# пути деплоя. Если демон вдруг древний и ругнётся на --mount — вернуть
# директиву первой строкой файла.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
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