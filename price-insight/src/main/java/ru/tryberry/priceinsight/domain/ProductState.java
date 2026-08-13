package ru.tryberry.priceinsight.domain;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;

/**
 * Состояние одного товара в state store: сегменты цены плюс агрегаты, которые
 * нельзя восстановить после вычистки хвоста.
 *
 * <p><b>Почему сегменты, а не точки.</b> В {@code price_history} хранение
 * change-only: запись появляется только когда цена изменилась. Стабильный товар
 * может иметь одну запись за сорок дней. Поэтому «обычная цена» — это медиана
 * ПО ВРЕМЕНИ (где товар провёл половину окна), а не по числу записей. Считать её
 * можно только зная длительность каждого интервала, отсюда сегменты.
 *
 * <p><b>Почему {@code observedSince} и {@code minAll} лежат отдельно.</b>
 * Сегменты старше окна удаляются {@link #prune}, иначе стейт растёт без предела.
 * Но «минимум за всё наблюдение» и «когда начали наблюдать» по усечённому списку
 * уже не восстановить — их надо накапливать по ходу.
 */
public final class ProductState {

    /** Глубина хранения сегментов. Всё, что старше, вычищается. */
    public static final Duration RETENTION = Duration.ofDays(90);

    private final List<Segment> segments = new ArrayList<>();
    private Instant observedSince;
    private double minAll;
    /**
     * Момент последнего применённого события. Отдельным полем, а не по открытому
     * сегменту: когда товара нет в наличии, открытого сегмента НЕТ, и опоздавшее
     * событие проехало бы мимо проверки и вставило сегмент задним числом.
     */
    private Instant lastEventAt;
    /**
     * Последнее опубликованное значение. Нужно, чтобы не сыпать в выходной топик
     * одинаковые вердикты на каждом тике пунктуатора — сравниваем и молчим, если
     * ничего не изменилось.
     */
    private HonestPrice lastEmitted;

    /**
     * Полуинтервал {@code [from, to)} с постоянной ценой. {@code to == null} —
     * сегмент ещё открыт, товар держит эту цену прямо сейчас.
     */
    public record Segment(double price, Instant from, Instant to) {

        /** Длительность части сегмента, попадающей в {@code [windowStart, now]}. */
        Duration durationInWindow(Instant windowStart, Instant now) {
            Instant start = from.isAfter(windowStart) ? from : windowStart;
            Instant end = to != null && to.isBefore(now) ? to : now;
            return end.isAfter(start) ? Duration.between(start, end) : Duration.ZERO;
        }

        boolean overlaps(Instant windowStart, Instant now) {
            return (to == null || to.isAfter(windowStart)) && !from.isAfter(now);
        }
    }

    /**
     * Применяет событие. Закрывает открытый сегмент моментом события и, если
     * цена наблюдаема, открывает новый. Возвращает {@code false}, если событие
     * не изменило состояние и писать его обратно в store незачем.
     *
     * <p>Событие «нет в наличии» ({@code newPrice == 0}) закрывает сегмент и
     * НЕ открывает следующий — образуется разрыв. Без этого время отсутствия
     * засчиталось бы как время по последней известной цене и утянуло бы медиану.
     *
     * <p>События приходят по одной партиции и по одному товару, но порядок в
     * пределах партиции гарантирован только по offset, не по {@code recordedAt}.
     * Событие старше последнего известного момента игнорируется: пересобирать
     * историю задним числом дороже, чем пропустить редкий выброс, а источник
     * правды всё равно остаётся в Postgres.
     */
    public boolean apply(PriceEvent event) {
        Instant at = event.recordedAt();
        if (at == null) {
            return false;
        }
        if (lastEventAt != null && !at.isAfter(lastEventAt)) {
            return false; // опоздавшее или дублирующее событие — игнорируем
        }
        lastEventAt = at;
        if (observedSince == null) {
            observedSince = at;
        }
        Segment open = openSegment();
        // Цена не изменилась — ПРОДОЛЖАЕМ открытый сегмент, а не режем новый.
        // Это не оптимизация, а условие совпадения с Go: price-events публикуется
        // на КАЖДОМ скрейпе (cmd/scraper/main.go), а price_history пишется только
        // при СМЕНЕ цены. Без склейки на стабильном товаре набегает по сегменту на
        // скрейп: у reseller с минутным кадансом это ~130 000 сегментов на товар за
        // 90 дней retention, и каждое событие переписывало бы весь снимок в RocksDB
        // и в changelog-топик. Со склейкой размер стейта равен числу реальных смен
        // цены, то есть числу строк price_history — ровно как в Go.
        if (open != null && event.hasPrice() && samePrice(open.price(), event.newPrice())) {
            prune(at);
            return true;
        }
        if (open != null) {
            segments.set(segments.size() - 1, new Segment(open.price(), open.from(), at));
        }
        if (event.hasPrice()) {
            segments.add(new Segment(event.newPrice(), at, null));
            if (minAll <= 0 || event.newPrice() < minAll) {
                minAll = event.newPrice();
            }
        }
        prune(at);
        return true;
    }

    /**
     * Равенство цен с точностью до копейки — тот же критерий, по которому
     * Go-скрапер решает, писать ли новую строку в price_history
     * ({@code pricesEqual}, cmd/scraper/main.go). Сравнивать {@code double}
     * напрямую нельзя: 1499.99 приезжает из разных парсеров с разным хвостом.
     */
    private static boolean samePrice(double a, double b) {
        return Math.round(a * 100) == Math.round(b * 100);
    }

    /**
     * Удаляет сегменты, целиком вышедшие за {@link #RETENTION}. Возвращает
     * {@code true}, если что-то удалено — по этому признаку пунктуатор решает,
     * надо ли вообще писать состояние обратно. При {@code exactly_once_v2}
     * каждая запись в store уходит в changelog-топик, и безусловный put по всем
     * товарам на каждом тике — это лишний трафик, растущий вместе с каталогом.
     */
    public boolean prune(Instant now) {
        Instant cutoff = now.minus(RETENTION);
        return segments.removeIf(s -> s.to() != null && !s.to().isAfter(cutoff));
    }

    /**
     * Агрегаты на момент {@code now}. Считаются по сегментам, обрезанным окном,
     * поэтому вызов в разные моменты даёт разный результат даже без новых
     * событий — окно едет само. Из-за этого нужен {@code Punctuator}.
     */
    public PriceStats stats(Instant now) {
        if (observedSince == null || segments.isEmpty()) {
            return PriceStats.empty();
        }
        return new PriceStats(
                minInWindow(now, Duration.ofDays(30)),
                weightedMedian(now, Duration.ofDays(30)),
                minInWindow(now, Duration.ofDays(90)),
                minAll,
                segmentsInWindow(now, Duration.ofDays(30)),
                observedSince,
                true);
    }

    /**
     * Минимум по окну. Сегмент учитывается по факту пересечения с окном, БЕЗ
     * требования ненулевой длительности: только что открытый сегмент в момент
     * своего же события длится ноль, и отбрасывать его нельзя — иначе при падении
     * цены до 700 минимум за 30 дней отрапортует старую 1000. В Go то же самое:
     * «current уже записан в price_history к моменту оценки, поэтому он входит в
     * min/median» (honest_price.go).
     *
     * <p>Из медианы такой сегмент выпадает сам — она взвешена по времени, и вес
     * у него нулевой. Это не расхождение, а разная природа двух метрик.
     */
    private double minInWindow(Instant now, Duration window) {
        Instant start = now.minus(window);
        double min = 0;
        for (Segment s : segments) {
            if (!s.overlaps(start, now)) {
                continue;
            }
            if (min <= 0 || s.price() < min) {
                min = s.price();
            }
        }
        return min;
    }

    private int segmentsInWindow(Instant now, Duration window) {
        Instant start = now.minus(window);
        return (int) segments.stream().filter(s -> s.overlaps(start, now)).count();
    }

    /**
     * Медиана, взвешенная по длительности: сортируем сегменты по цене и идём,
     * накапливая время, пока не пройдём половину суммарной длительности окна.
     * Медиана, а не среднее — устойчива к выбросу и к попытке накрутить нашу же
     * историю одним скачком цены.
     *
     * <p>Порог — НИЖНЯЯ взвешенная медиана: берём первый сегмент, у которого
     * накопленное время достигло половины ({@code acc >= total/2}), а не
     * превысило её. Это дословно условие Go-запроса {@code cum >= tot / 2.0}
     * (price_history.go). Разница видна ровно на равенстве: при двух сегментах
     * по 50 % времени Go отдаёт дешёвый, а {@code acc > total/2} отдал бы
     * дорогой — и вердикт «обычная цена» превратился бы в «выше обычной».
     * Умножение вместо деления, чтобы не терять нечётный миллисекунд на
     * целочисленном делении.
     */
    private double weightedMedian(Instant now, Duration window) {
        Instant start = now.minus(window);
        record Weighted(double price, long millis) {}

        List<Weighted> weighted = new ArrayList<>();
        long total = 0;
        for (Segment s : segments) {
            long millis = s.durationInWindow(start, now).toMillis();
            if (millis > 0) {
                weighted.add(new Weighted(s.price(), millis));
                total += millis;
            }
        }
        if (total == 0) {
            return 0;
        }
        weighted.sort(Comparator.comparingDouble(Weighted::price));
        long acc = 0;
        for (Weighted w : weighted) {
            acc += w.millis();
            if (acc * 2 >= total) {
                return w.price();
            }
        }
        return weighted.get(weighted.size() - 1).price();
    }

    private Segment openSegment() {
        if (segments.isEmpty()) {
            return null;
        }
        Segment last = segments.get(segments.size() - 1);
        return last.to() == null ? last : null;
    }

    public List<Segment> segments() {
        return List.copyOf(segments);
    }

    public Instant observedSince() {
        return observedSince;
    }

    /**
     * Цена открытого сегмента, то есть сколько товар стоит прямо сейчас.
     * Ноль означает «нет в наличии»: последний сегмент закрыт и новый не открыт.
     */
    public double currentPrice() {
        Segment open = openSegment();
        return open == null ? 0 : open.price();
    }

    /**
     * Отмечает значение как опубликованное. Возвращает {@code true}, если оно
     * отличается от прошлого и его действительно надо отправить.
     */
    public boolean shouldEmit(HonestPrice candidate) {
        if (candidate.sameAsPublished(lastEmitted)) {
            return false;
        }
        lastEmitted = candidate;
        return true;
    }

    /**
     * Снимок для сериализации в state store. Формат — контракт с
     * changelog-топиком: несовместимое изменение сделает уже накопленный стейт
     * нечитаемым, и восстанавливать придётся проигрыванием price-events с
     * начала. Поэтому поля только дописываются, старые не удаляются.
     */
    public record Snapshot(
            List<Segment> segments,
            Instant observedSince,
            double minAll,
            HonestPrice lastEmitted,
            Instant lastEventAt
    ) {}

    public Snapshot toSnapshot() {
        return new Snapshot(List.copyOf(segments), observedSince, minAll, lastEmitted, lastEventAt);
    }

    public static ProductState fromSnapshot(Snapshot snapshot) {
        ProductState state = new ProductState();
        if (snapshot == null) {
            return state;
        }
        if (snapshot.segments() != null) {
            state.segments.addAll(snapshot.segments());
        }
        state.observedSince = snapshot.observedSince();
        state.minAll = snapshot.minAll();
        state.lastEmitted = snapshot.lastEmitted();
        state.lastEventAt = snapshot.lastEventAt();
        return state;
    }
}
