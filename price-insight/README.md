# price-insight

Материализация «честной цены» из потока `price-events`. Единственный сервис
проекта на JVM — обоснование выбора, границы и отвергнутые альтернативы в
[`docs/PRICE-INSIGHT-JAVA.md`](../docs/PRICE-INSIGHT-JAVA.md).

```
price-events ──► [price-insight] ──► price-verdict ──► price_insight (Postgres)
   (Go, есть)      Kafka Streams        compacted          читает notifier
```

Коротко: `PriceHistoryRepo.Stats` считает time-weighted медиану SQL-запросом на
каждое уведомление и каждый дайджест. Агрегаты при этом меняются
инкрементально, а пересчитываются целиком. Сервис держит по товару сегментное
состояние, обновляет его по потоку и публикует готовый вердикт — notifier
читает его за O(1) и откатывается на старый путь, если вердикт протух.

## Сборка и тесты

```bash
export JAVA_HOME=~/.local/tools/jdk-21.0.12+8
export PATH="$JAVA_HOME/bin:$HOME/.local/tools/apache-maven-3.9.9/bin:$PATH"

mvn test          # доменные тесты + топология на TopologyTestDriver, без Docker
mvn verify        # плюс сквозной тест на Testcontainers (нужен Docker)
mvn package       # jar
```

Сквозной тест пропускается сам, если Docker недоступен. В WSL интеграция
включается в настройках Docker Desktop.

## Структура

| Пакет | Что внутри |
|---|---|
| `domain` | `ProductState` — сегменты и инкрементальная агрегация; `HonestPriceRules` — порт правил вердикта из Go |
| `stream` | топология, `Processor`, формат выходного сообщения |
| `projection` | батчевый консьюмер `price-verdict` → upsert в `price_insight` |
| `serde` | JSON-сериализация сообщений и состояния |
| `config` | настройки, `exactly_once_v2`, метрики Streams в Micrometer |

## Что стоит знать, прежде чем править

**Формат состояния — контракт.** `ProductState.Snapshot` сериализуется в state
store и в changelog-топик. Несовместимое изменение сделает накопленный стейт
нечитаемым, восстанавливать придётся проигрыванием `price-events` с начала.
Поля только дописываются.

**Коды вердиктов — контракт.** `PriceVerdict.code()` уезжает в Postgres и
читается Go-стороной. Порядок совпадает с iota в `internal/domain/honest_price.go`,
менять нельзя.

**Нулевая цена — это «нет в наличии», а не «бесплатно».** Такое событие
закрывает сегмент и не открывает новый: время отсутствия не должно засчитываться
как время по последней известной цене.

**Минимум и медиана считаются по-разному.** Только что открытый сегмент длится
ноль и потому не влияет на медиану (она взвешена по времени), но обязан
учитываться в минимуме — иначе при падении цены сервис отрапортует старый
минимум. Так же устроено в Go.

## Переменные окружения

| Переменная | Назначение | По умолчанию |
|---|---|---|
| `KAFKA_BROKERS` | брокеры | `localhost:9092` |
| `POSTGRES_DSN` | JDBC URL | `jdbc:postgresql://localhost:5432/tryberry` |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` | доступ к БД | — |
| `STATE_DIR` | каталог state store | `/var/lib/price-insight` |
| `JAVA_OPTS` | опции JVM | см. Dockerfile |

## Метрики

`/actuator/prometheus` на порту 8092: лаг по партициям, размер стейта, время
обработки записи. Скрейпится тем же Prometheus, что и остальной проект.
