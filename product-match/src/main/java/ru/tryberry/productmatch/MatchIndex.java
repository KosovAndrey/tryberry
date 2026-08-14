package ru.tryberry.productmatch;

import org.apache.lucene.document.Document;
import org.apache.lucene.document.Field;
import org.apache.lucene.document.StringField;
import org.apache.lucene.document.TextField;
import org.apache.lucene.index.DirectoryReader;
import org.apache.lucene.index.IndexWriter;
import org.apache.lucene.index.IndexWriterConfig;
import org.apache.lucene.index.Term;
import org.apache.lucene.search.BooleanClause;
import org.apache.lucene.search.BooleanQuery;
import org.apache.lucene.search.BoostQuery;
import org.apache.lucene.search.IndexSearcher;
import org.apache.lucene.search.Query;
import org.apache.lucene.search.ScoreDoc;
import org.apache.lucene.search.TermQuery;
import org.apache.lucene.search.TopDocs;
import org.apache.lucene.store.ByteBuffersDirectory;
import org.apache.lucene.util.QueryBuilder;

import java.io.IOException;
import java.util.ArrayList;
import java.util.List;

/**
 * Индекс каталога и поиск кандидатов. Прототип: индекс держится в памяти —
 * корпус тут тысячи документов, единицы мегабайт (docs/PRODUCT-MATCH-JVM.md §7,
 * там же довод против Elasticsearch).
 *
 * <p><b>Это только ЭТАП ОТБОРА.</b> Он оптимизирует recall: важно принести
 * нужного кандидата в топ-K, а точность здесь не важна вовсе — её обеспечивает
 * отдельный слой решения с правом отказа (§9). Смешивать два этапа нельзя: у
 * них противоположные метрики.
 */
public final class MatchIndex implements AutoCloseable {

    private static final String F_TITLE = "title";
    private static final String F_MODEL = "model";
    private static final String F_BRAND = "brand";
    private static final String F_MP = "mp";
    private static final String F_URL = "url";

    /**
     * Вес модельного кода против слов названия. Совпадение «SM-A546E» — почти
     * детерминанта, совпадение слова «чёрный» — почти шум.
     */
    private static final float MODEL_BOOST = 8f;

    private final ByteBuffersDirectory dir = new ByteBuffersDirectory();
    private final ProductAnalyzer analyzer = new ProductAnalyzer();
    private final QueryBuilder queryBuilder = new QueryBuilder(analyzer);
    private final DirectoryReader reader;
    private final IndexSearcher searcher;
    private final List<Item> items;

    public MatchIndex(List<Item> items) throws IOException {
        this.items = items;
        try (IndexWriter w = new IndexWriter(dir, new IndexWriterConfig(analyzer))) {
            for (Item it : items) {
                Document d = new Document();
                d.add(new TextField(F_TITLE, it.name(), Field.Store.NO));
                for (String m : it.modelTokens()) {
                    d.add(new StringField(F_MODEL, m, Field.Store.NO));
                }
                d.add(new StringField(F_BRAND, Brands.normalize(it), Field.Store.NO));
                d.add(new StringField(F_MP, it.marketplace(), Field.Store.NO));
                d.add(new StringField(F_URL, it.url(), Field.Store.YES));
                w.addDocument(d);
            }
        }
        this.reader = DirectoryReader.open(dir);
        this.searcher = new IndexSearcher(reader);
    }

    /**
     * Кандидаты на «тот же товар» для {@code probe}, СТРОГО с других площадок.
     *
     * <p>Бренд — жёсткий фильтр, а не слагаемое скора: разные бренды не бывают
     * одним товаром, и пускать их в выдачу значит тратить места в топ-K.
     * Я.Маркет отдаёт бренд структурно (`vendorName`), так что фильтр надёжен.
     */
    public List<Hit> search(Item probe, int k) throws IOException {
        BooleanQuery.Builder q = new BooleanQuery.Builder();

        Query title = queryBuilder.createBooleanQuery(F_TITLE, probe.name(), BooleanClause.Occur.SHOULD);
        if (title != null) {
            q.add(title, BooleanClause.Occur.SHOULD);
        }
        for (String m : probe.modelTokens()) {
            q.add(new BoostQuery(new TermQuery(new Term(F_MODEL, m)), MODEL_BOOST),
                    BooleanClause.Occur.SHOULD);
        }

        String brand = Brands.normalize(probe);
        if (!brand.isEmpty() && !brand.equals("?")) {
            q.add(new TermQuery(new Term(F_BRAND, brand)), BooleanClause.Occur.FILTER);
        }
        // Своя площадка исключается: ищем ТОТ ЖЕ товар у ДРУГИХ.
        q.add(new TermQuery(new Term(F_MP, probe.marketplace())), BooleanClause.Occur.MUST_NOT);

        BooleanQuery built = q.build();
        if (built.clauses().stream().noneMatch(c -> c.getOccur() == BooleanClause.Occur.SHOULD)) {
            return List.of();
        }

        TopDocs top = searcher.search(built, k);
        List<Hit> out = new ArrayList<>(top.scoreDocs.length);
        int rank = 0;
        for (ScoreDoc sd : top.scoreDocs) {
            out.add(new Hit(items.get(sd.doc), sd.score, ++rank));
        }
        return out;
    }

    /** Кандидат: сама позиция, оценка BM25 и место в выдаче. */
    public record Hit(Item item, float score, int rank) {}

    @Override
    public void close() throws IOException {
        reader.close();
        dir.close();
        analyzer.close();
    }
}
