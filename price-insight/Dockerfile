# ── Builder ──────────────────────────────────────────────────────────────────
FROM maven:3.9-eclipse-temurin-21 AS builder

WORKDIR /build

# Зависимости отдельным слоем — pom меняется куда реже исходников.
COPY pom.xml .
RUN mvn -B -q dependency:go-offline

COPY src ./src
# Тесты гоняются ЗДЕСЬ, при сборке образа. Раньше стояло -DskipTests с
# комментарием «тесты гоняет CI» — CI в проекте не было и быть не может
# (GitLab требует верифицированный аккаунт), то есть Java-тесты не запускались
# нигде и никогда. Машина разработчика их тоже не прогонит: JVM там нет,
# единственное место с Docker и Maven — сборка этого образа.
#
# Testcontainers сборку не ломают: IntegrationTest помечен
# @Testcontainers(disabledWithoutDocker = true) и без демона внутри билд-слоя
# просто пропускается. Юнит-тесты домена и прогон топологии на
# TopologyTestDriver брокера не требуют и идут полностью.
#
# Цена: красный тест теперь ломает `docker compose build price-insight`. Это и
# есть смысл — деплою Go-сервисов (make deploy) он не мешает, price-insight там
# не собирается.
RUN mvn -B package

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM eclipse-temurin:21-jre-alpine

# libstdc++ — для нативной части RocksDB. Образ на musl (alpine), rocksdbjni
# подхватывает musl-сборку .so, но она линкуется с libstdc++ динамически, и без
# пакета падает при инициализации первого store — то есть уже после старта
# Spring, невнятной ошибкой загрузки библиотеки.
RUN apk add --no-cache libstdc++ \
    && addgroup -S app && adduser -S app -G app \
    && mkdir -p /var/lib/price-insight && chown app:app /var/lib/price-insight

WORKDIR /app
COPY --from=builder /build/target/price-insight-*.jar /app/app.jar
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

USER app

# MaxRAMPercentage вместо -Xmx: heap считается от лимита контейнера, и при
# изменении mem_limit не надо править Dockerfile. Доля небольшая намеренно —
# RocksDB держит block cache ВНЕ heap, и ему нужно оставить место.
#
# Бюджет на лимит 1 ГБ: heap 40 % ≈ 410 МБ, RocksDB ≤ 160 МБ (потолок задан
# в BoundedMemoryRocksDBConfig), остальное — metaspace, стеки потоков, буферы
# клиента Kafka и code cache. Фактический расход надо померить под реальным
# потоком: смета составлена по умолчаниям, а не по замеру.
ENV JAVA_OPTS="-XX:MaxRAMPercentage=40 -XX:+UseG1GC -XX:MaxGCPauseMillis=200"

# Стейт переживает перезапуск контейнера — иначе Streams будет каждый раз
# восстанавливать его из changelog-топика.
VOLUME ["/var/lib/price-insight"]

EXPOSE 8092
ENTRYPOINT ["/app/docker-entrypoint.sh"]
