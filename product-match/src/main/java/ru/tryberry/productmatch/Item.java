package ru.tryberry.productmatch;

import java.util.LinkedHashSet;
import java.util.Set;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Позиция каталога: строка TSV от {@code cmd/title-probe}.
 *
 * <p>Здесь же живёт доменная нормализация — та самая часть, где настоящая
 * инженерия, а не подключение библиотеки (docs/PRODUCT-MATCH-JVM.md §9).
 * Правила извлечения ОДИНАКОВЫ с {@code scripts/make-pairs.py}: если признаки
 * разъедутся, сравнение baseline с индексом перестанет быть честным.
 */
public record Item(String marketplace, String article, String brand,
                   double price, String name, String url) {

    /**
     * Артикул производителя в скобках: «JURA E8 Piano black EC (15584)».
     * Встречен живьём на карточке Я.Маркета, см. §3 дока.
     */
    private static final Pattern PARENS = Pattern.compile("\\((\\d{4,7})\\)");

    /** Латинский токен, содержащий И буквы, И цифры: SM-A546E, KQJHQ01ZM, VCR04W. */
    private static final Pattern TOKEN = Pattern.compile("\\b[A-Za-z][A-Za-z0-9-]{2,19}\\b");

    /**
     * Объём памяти. Написаний четыре, и каждая площадка добавила своё:
     * «8/256 ГБ» (Ozon), «8 ГБ/256 ГБ» (Я.Маркет), «8/256» без единицы,
     * «256+8 ГБ» и «12+512 Гб» (WB — через плюс и в ОБРАТНОМ порядке).
     * Поэтому разбираем любую пару чисел через «/» или «+» и раскладываем по
     * величине: оперативки больше, чем ПЗУ, не бывает.
     */
    //
    // UNICODE_CHARACTER_CLASS обязателен: в Java `\b` по умолчанию ASCII-only, и
    // граница слова ПОСЛЕ кириллического «гб» не срабатывает — объём не
    // распознавался бы почти нигде, а это главное жёсткое ограничение против
    // ошибок типа C. В Python те же шаблоны работали (там `\b` юникодный), из-за
    // чего расхождение и было незаметным до тестов.
    private static final Pattern MEM_PAIR = Pattern.compile(
            "\\b(\\d{1,4})\\s*(?:гб|gb)?\\s*[/+]\\s*(\\d{1,4})\\s*(?:гб|gb)?\\b",
            Pattern.CASE_INSENSITIVE | Pattern.UNICODE_CHARACTER_CLASS);
    private static final Pattern MEM_ONE = Pattern.compile(
            "\\b(\\d{2,4})\\s*(?:гб|gb)\\b",
            Pattern.CASE_INSENSITIVE | Pattern.UNICODE_CHARACTER_CLASS);

    /**
     * ПЯТЫЙ формат: через ПРОБЕЛ, без разделителя — «Redmi 15C 8 256 Черный».
     * Найден на живой разметке: подсказка врала «память совпал» там, где было
     * 4/256 против 8/256. Голый пробел даёт много ложных срабатываний (в
     * названиях полно чисел), поэтому требуем ПРАВДОПОДОБНЫЕ значения: столько
     * ОЗУ и ПЗУ реально бывает, а «15 8» из «Note 15 8» — нет.
     */
    private static final Pattern MEM_SPACE = Pattern.compile(
            "\\b(\\d{1,2})\\s+(\\d{2,4})\\s*(?:гб|gb)?\\b",
            Pattern.CASE_INSENSITIVE | Pattern.UNICODE_CHARACTER_CLASS);
    private static final Set<Integer> RAM_VALUES = Set.of(2, 3, 4, 6, 8, 12, 16, 18, 24);
    private static final Set<Integer> ROM_VALUES = Set.of(16, 32, 64, 128, 256, 512, 1024);

    /**
     * Модельные коды из названия. Индексируются ОТДЕЛЬНЫМ полем с точным
     * совпадением: совпадение `SM-A546E` должно весить кратно больше слова
     * «чёрный», а внутри общего текстового поля этого не выразить.
     */
    public Set<String> modelTokens() {
        Set<String> out = new LinkedHashSet<>();
        Matcher p = PARENS.matcher(name);
        while (p.find()) {
            out.add(p.group(1));
        }
        Matcher t = TOKEN.matcher(name);
        while (t.find()) {
            String tok = t.group();
            long digits = tok.chars().filter(Character::isDigit).count();
            if (digits >= 2) {
                out.add(tok.toLowerCase());
            }
        }
        return out;
    }

    /**
     * Квалификаторы имени модели. Тот же тип ошибки, что объём памяти, только по
     * названию: «Pro» против «Pro Max», «Note 13» против «13», «Lite» против
     * базовой — РАЗНЫЕ товары с почти одинаковыми названиями. Решаться должно
     * жёстко, а не похожестью.
     *
     * <p>Список ОДИН И ТОТ ЖЕ с {@code scripts/make-pairs.py}: разъедутся —
     * baseline и индекс начнут сравниваться на разных признаках.
     */
    private static final Set<String> VARIANT_WORDS = Set.of(
            "pro", "про", "max", "макс", "plus", "плюс", "ultra", "ультра",
            "lite", "лайт", "mini", "мини", "note", "нот", "neo", "se", "fe",
            "prime", "power", "turbo", "active", "young");
    // «5G» СПЕЦИАЛЬНО не входит: площадки его часто опускают, отсутствие не
    // означает отличия — ведёт себя как «Ростест», а не как «Pro». Выяснилось
    // на живой разметке (см. scripts/make-pairs.py).

    /** Квалификаторы модели, найденные в названии. */
    public Set<String> variantTokens() {
        Set<String> out = new LinkedHashSet<>();
        for (String t : name.toLowerCase(java.util.Locale.ROOT).split("[^\\p{L}\\p{N}]+")) {
            if (VARIANT_WORDS.contains(t)) {
                out.add(t);
            }
        }
        return out;
    }

    /**
     * Квалификатор варианта — «8/256». Это НЕ идентичность, а то, чем варианты
     * РАЗЛИЧАЮТСЯ: несовпадение здесь обязано быть отказом, а не минусом к
     * скору (§9, тип ошибки C). Пусто — квалификатор не распознан.
     */
    public String storage() {
        Matcher m = MEM_PAIR.matcher(name);
        if (m.find()) {
            int a = Integer.parseInt(m.group(1));
            int b = Integer.parseInt(m.group(2));
            return Math.min(a, b) + "/" + Math.max(a, b);
        }
        Matcher sp = MEM_SPACE.matcher(name);
        while (sp.find()) {
            int a = Integer.parseInt(sp.group(1));
            int b = Integer.parseInt(sp.group(2));
            if (RAM_VALUES.contains(a) && ROM_VALUES.contains(b)) {
                return a + "/" + b;
            }
        }
        Matcher one = MEM_ONE.matcher(name);
        if (one.find()) {
            return "?/" + one.group(1);
        }
        return "";
    }
}
