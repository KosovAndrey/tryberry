package ru.tryberry.productmatch;

import java.io.IOException;
import java.io.PrintWriter;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.HashSet;
import java.util.Set;

/**
 * Прототип матчера: строит индекс по корпусу и считает ДВА числа, ради которых
 * он и написан (docs/PRODUCT-MATCH-JVM.md §9, §10).
 *
 * <p><b>1. Оценка на каждую пару</b> размеченного набора — чтобы сравнение с
 * триграммным baseline шло на ОДНОМ И ТОМ ЖЕ наборе и теми же метриками.
 * Сравнивать на разных выборках нечестно, а условие отказа от затеи записано
 * заранее и опирается именно на этот разрыв.
 *
 * <p><b>2. recall@K</b> — доля настоящих пар, попавших в топ-K кандидатов. Это
 * решающее число для вопроса «нужны ли эмбеддинги»: если BM25 и так приносит
 * нужного кандидата почти всегда, векторам негде добавить ценность, потому что
 * слой решения всё равно перебирает всех K.
 *
 * <pre>
 * mvn -q compile exec:java или:
 * java -cp target/classes:… ru.tryberry.productmatch.Main \
 *      --items testdata/product-match/items-smartphones.tsv \
 *      --pairs testdata/product-match/gold-smartphones.tsv \
 *      --out   /tmp/lucene-scored.tsv
 * </pre>
 */
public final class Main {

    public static void main(String[] args) throws IOException {
        Map<String, String> opt = parseArgs(args);
        Path itemsPath = Path.of(opt.getOrDefault("items", "testdata/product-match/items-smartphones.tsv"));
        Path pairsPath = Path.of(opt.getOrDefault("pairs", "testdata/product-match/gold-smartphones.tsv"));
        Path outPath = Path.of(opt.getOrDefault("out", "lucene-scored.tsv"));
        int k = Integer.parseInt(opt.getOrDefault("k", "50"));

        List<Item> items = loadItems(itemsPath);
        Map<String, Item> byUrl = new HashMap<>();
        for (Item it : items) {
            byUrl.put(it.url(), it);
        }
        System.out.printf("позиций в корпусе: %d, площадок: %d%n", items.size(),
                items.stream().map(Item::marketplace).distinct().count());

        List<String[]> pairs = loadPairs(pairsPath);
        System.out.printf("пар в наборе: %d%n", pairs.size());

        // Режим предложения кандидатов. Нужен, потому что золотой набор отобрало
        // ТРИГРАММНОЕ сито, и потому он структурно не способен показать случаи,
        // где индекс находит то, что триграммы пропустили, — а это главное
        // преимущество Lucene («iPhone15» против «iPhone 15»). Размечать такой
        // набор значит измерить только точность на чужих кандидатах.
        if (opt.containsKey("propose")) {
            propose(items, pairs, Path.of(opt.get("propose")),
                    Integer.parseInt(opt.getOrDefault("propose-top", "3")));
            return;
        }

        try (MatchIndex index = new MatchIndex(items);
             PrintWriter out = new PrintWriter(Files.newBufferedWriter(outPath, StandardCharsets.UTF_8))) {

            out.println("id\tlabel\tmem\ttrgm_sim\tlucene_score\tlucene_rank");

            int labeled = 0, positives = 0, foundInTopK = 0;
            long t0 = System.currentTimeMillis();

            for (String[] p : pairs) {
                Item a = byUrl.get(p[P_URL_A]);
                Item b = byUrl.get(p[P_URL_B]);
                if (a == null || b == null) {
                    continue; // пара из другого корпуса — пропускаем молча
                }
                // Максимум по двум направлениям: пара может находиться только в
                // одну сторону, а нам важно, находится ли она вообще.
                Hit best = better(find(index, a, b, k), find(index, b, a, k));

                String label = p[P_LABEL].trim();
                if (label.equals("0") || label.equals("1")) {
                    labeled++;
                    if (label.equals("1")) {
                        positives++;
                        if (best.rank > 0) {
                            foundInTopK++;
                        }
                    }
                }
                out.printf("%s\t%s\t%s\t%s\t%.4f\t%d%n",
                        p[P_ID], label, p[P_MEM], p[P_SIM], best.score, best.rank);
            }
            long ms = System.currentTimeMillis() - t0;

            System.out.printf("оценено пар: %d за %d мс%n", pairs.size(), ms);
            System.out.printf("результат: %s%n", outPath);
            System.out.println();

            if (labeled == 0) {
                System.out.println("В наборе нет размеченных строк (колонка label пуста).");
                System.out.println("Оценки посчитаны, но recall и precision без разметки не считаются.");
                System.out.println("Разметка: 1 — тот же товар, 0 — разные. Правила — §10 дока.");
                return;
            }

            System.out.printf("размечено: %d, положительных: %d%n", labeled, positives);
            if (positives > 0) {
                System.out.printf("recall@%d = %.3f (%d из %d настоящих пар попали в топ-%d)%n",
                        k, (double) foundInTopK / positives, foundInTopK, positives, k);
                System.out.println();
                System.out.println("Как читать: recall@K — качество ЭТАПА ОТБОРА, точность здесь");
                System.out.println("не важна. Если он высок, эмбеддинги добавить нечего (§9);");
                System.out.println("если низок — разбирать промахи по типам A/B, там их место.");
            }
        }
    }

    /**
     * Выписывает кандидатов, которых предложил бы ИНДЕКС, исключая уже
     * присутствующих в наборе. Формат — url_a/url_b плюс оценка, дальше их
     * дополняет и приводит к формату разметки scripts/add-lucene-candidates.py.
     */
    private static void propose(List<Item> items, List<String[]> existing,
                                Path out, int top) throws IOException {
        Set<String> seen = new HashSet<>();
        for (String[] p : existing) {
            seen.add(key(p[P_URL_A], p[P_URL_B]));
        }
        int written = 0;
        try (MatchIndex index = new MatchIndex(items);
             PrintWriter w = new PrintWriter(Files.newBufferedWriter(out, StandardCharsets.UTF_8))) {
            w.println("url_a\turl_b\tlucene_score\tlucene_rank");
            for (Item a : items) {
                for (MatchIndex.Hit h : index.search(a, top)) {
                    String k = key(a.url(), h.item().url());
                    if (seen.add(k)) {
                        w.printf("%s\t%s\t%.4f\t%d%n",
                                a.url(), h.item().url(), h.score(), h.rank());
                        written++;
                    }
                }
            }
        }
        System.out.printf("предложено новых кандидатов: %d → %s%n", written, out);
        System.out.println("Дальше: scripts/add-lucene-candidates.py отфильтрует те,");
        System.out.println("что триграммы оценили НИЗКО — там и живёт преимущество индекса.");
    }

    /** Ключ пары без учёта направления. */
    private static String key(String a, String b) {
        return a.compareTo(b) <= 0 ? a + "\u0000" + b : b + "\u0000" + a;
    }

    // ── Разбор набора пар (формат scripts/make-pairs.py) ─────────────────────
    private static final int P_LABEL = 0, P_ID = 1, P_SIM = 2, P_MEM = 3;
    private static int P_URL_A, P_URL_B;

    private record Hit(float score, int rank) {}

    private static Hit find(MatchIndex index, Item probe, Item target, int k) throws IOException {
        for (MatchIndex.Hit h : index.search(probe, k)) {
            if (h.item().url().equals(target.url())) {
                return new Hit(h.score(), h.rank());
            }
        }
        return new Hit(0f, 0);
    }

    private static Hit better(Hit x, Hit y) {
        if (x.rank == 0) return y;
        if (y.rank == 0) return x;
        return x.score >= y.score ? x : y;
    }

    private static List<Item> loadItems(Path path) throws IOException {
        List<Item> out = new ArrayList<>();
        List<String> lines = Files.readAllLines(path, StandardCharsets.UTF_8);
        for (int i = 1; i < lines.size(); i++) {
            String[] p = lines.get(i).split("\t", -1);
            if (p.length < 6) {
                continue;
            }
            double price = 0;
            try {
                price = Double.parseDouble(p[3]);
            } catch (NumberFormatException ignored) {
                // цена для отбора не нужна — она понадобится слою решения
            }
            out.add(new Item(p[0], p[1], p[2], price, p[4], p[5]));
        }
        return out;
    }

    private static List<String[]> loadPairs(Path path) throws IOException {
        List<String> lines = Files.readAllLines(path, StandardCharsets.UTF_8);
        String[] header = lines.get(0).split("\t", -1);
        for (int i = 0; i < header.length; i++) {
            if (header[i].equals("url_a")) P_URL_A = i;
            if (header[i].equals("url_b")) P_URL_B = i;
        }
        List<String[]> out = new ArrayList<>();
        for (int i = 1; i < lines.size(); i++) {
            String[] p = lines.get(i).split("\t", -1);
            if (p.length == header.length) {
                out.add(p);
            }
        }
        return out;
    }

    private static Map<String, String> parseArgs(String[] args) {
        Map<String, String> out = new HashMap<>();
        for (int i = 0; i + 1 < args.length; i += 2) {
            out.put(args[i].replaceFirst("^--", ""), args[i + 1]);
        }
        return out;
    }
}
