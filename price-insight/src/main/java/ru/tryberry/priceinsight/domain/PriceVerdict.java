package ru.tryberry.priceinsight.domain;

/**
 * Вердикт по текущей цене. Числовые коды совпадают с iota из
 * {@code internal/domain/honest_price.go} — они уезжают в Postgres и читаются
 * Go-стороной, поэтому порядок менять нельзя, только дописывать в конец.
 */
public enum PriceVerdict {
    /** Данных мало — наблюдаем, ничего не утверждаем. */
    INSUFFICIENT(0),
    /** Не выше минимума за всё наблюдение. */
    LOWEST_EVER(1),
    /** Не выше минимума за 90 дней. */
    LOWEST_90(2),
    /** Не выше минимума за 30 дней. */
    LOWEST_30(3),
    /** Не выше медианы за 30 дней — обычная цена. */
    TYPICAL(4),
    /** Выше медианы за 30 дней — «скидка» завышена. */
    ABOVE_TYPICAL(5);

    private final int code;

    PriceVerdict(int code) {
        this.code = code;
    }

    public int code() {
        return code;
    }
}
