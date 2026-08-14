package ru.tryberry.productmatch;

import org.apache.lucene.analysis.Analyzer;
import org.apache.lucene.analysis.LowerCaseFilter;
import org.apache.lucene.analysis.TokenStream;
import org.apache.lucene.analysis.core.FlattenGraphFilter;
import org.apache.lucene.analysis.miscellaneous.WordDelimiterGraphFilter;
import org.apache.lucene.analysis.ru.RussianLightStemFilter;
import org.apache.lucene.analysis.standard.StandardTokenizer;

/**
 * Цепочка анализа названий. Это и есть ответ на вопрос «почему JVM»: в Go нет
 * ни русской морфологии сопоставимого качества, ни настраиваемой цепочки
 * фильтров (docs/PRODUCT-MATCH-JVM.md §6).
 *
 * <p>Порядок фильтров:
 * <ol>
 *   <li>{@link StandardTokenizer} — разбиение на слова;
 *   <li>{@link LowerCaseFilter};
 *   <li>{@link WordDelimiterGraphFilter} — рвёт «iPhone13» на «iphone» + «13»
 *       и «SM-A546E» на части, СОХРАНЯЯ оригинал: модельный код должен остаться
 *       целым токеном, иначе он перестанет отличать модели друг от друга;
 *   <li>{@link FlattenGraphFilter} — обязателен после graph-фильтра на
 *       индексации, иначе позиции токенов поедут;
 *   <li>{@link RussianLightStemFilter} — «очистителя» ↔ «очиститель». Взят
 *       лёгкий стеммер, а не Snowball: агрессивный резал бы и латинские
 *       «Pro»/«Air»/«Max», которые у нас часть имени модели.
 * </ol>
 */
public final class ProductAnalyzer extends Analyzer {

    /**
     * Флаги WordDelimiterGraph. GENERATE_* дают части, CATENATE_* — склейки,
     * PRESERVE_ORIGINAL оставляет исходный токен. Вместе это покрывает и
     * «iphone 13», и «iphone13», и «SM-A546E» одним полем.
     */
    private static final int WDF_FLAGS =
            WordDelimiterGraphFilter.GENERATE_WORD_PARTS
                    | WordDelimiterGraphFilter.GENERATE_NUMBER_PARTS
                    | WordDelimiterGraphFilter.CATENATE_NUMBERS
                    | WordDelimiterGraphFilter.SPLIT_ON_CASE_CHANGE
                    | WordDelimiterGraphFilter.SPLIT_ON_NUMERICS
                    | WordDelimiterGraphFilter.PRESERVE_ORIGINAL;

    @Override
    protected TokenStreamComponents createComponents(String fieldName) {
        StandardTokenizer src = new StandardTokenizer();
        TokenStream ts = new LowerCaseFilter(src);
        ts = new WordDelimiterGraphFilter(ts, WDF_FLAGS, null);
        ts = new FlattenGraphFilter(ts);
        ts = new RussianLightStemFilter(ts);
        return new TokenStreamComponents(src, ts);
    }
}
