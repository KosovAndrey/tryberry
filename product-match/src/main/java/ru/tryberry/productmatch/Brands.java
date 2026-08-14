package ru.tryberry.productmatch;

import java.util.Locale;
import java.util.Map;

/**
 * Нормализация бренда — ключ блокинга. Площадки пишут его и латиницей, и
 * кириллицей: без словаря «Сяоми» и «Xiaomi» никогда не встретятся, и товар
 * просто не попадёт в кандидаты.
 *
 * <p>Словарь ОДИН И ТОТ ЖЕ с {@code scripts/make-pairs.py}: если они разъедутся,
 * сравнение baseline с индексом пойдёт на разных выборках и перестанет быть
 * честным. Суб-бренды сведены к владельцу (Redmi/POCO → xiaomi, Galaxy →
 * samsung): на площадках они пишутся вперемешку.
 */
public final class Brands {

    private static final Map<String, String> ALIASES = Map.ofEntries(
            Map.entry("xiaomi", "xiaomi"), Map.entry("сяоми", "xiaomi"),
            Map.entry("ксиаоми", "xiaomi"), Map.entry("ксяоми", "xiaomi"),
            Map.entry("redmi", "xiaomi"), Map.entry("редми", "xiaomi"),
            Map.entry("poco", "xiaomi"), Map.entry("поко", "xiaomi"),
            Map.entry("samsung", "samsung"), Map.entry("самсунг", "samsung"),
            Map.entry("galaxy", "samsung"),
            Map.entry("apple", "apple"), Map.entry("эппл", "apple"),
            Map.entry("айфон", "apple"), Map.entry("iphone", "apple"),
            Map.entry("honor", "honor"), Map.entry("хонор", "honor"),
            Map.entry("huawei", "huawei"), Map.entry("хуавей", "huawei"),
            Map.entry("хуавэй", "huawei"),
            Map.entry("realme", "realme"), Map.entry("реалми", "realme"),
            Map.entry("tecno", "tecno"), Map.entry("текно", "tecno"),
            Map.entry("infinix", "infinix"), Map.entry("инфиникс", "infinix"),
            Map.entry("vivo", "vivo"), Map.entry("виво", "vivo"),
            Map.entry("oppo", "oppo"), Map.entry("оппо", "oppo"),
            Map.entry("zte", "zte"), Map.entry("nubia", "zte"),
            Map.entry("нубиа", "zte"),
            Map.entry("motorola", "motorola"), Map.entry("моторола", "motorola"),
            Map.entry("nothing", "nothing"),
            Map.entry("google", "google"), Map.entry("pixel", "google"));

    private Brands() {
    }

    /** Бренд из колонки, иначе — первый узнаваемый токен названия. */
    public static String normalize(Item item) {
        String b = item.brand() == null ? "" : item.brand().trim().toLowerCase(Locale.ROOT);
        String mapped = ALIASES.get(b);
        if (mapped != null) {
            return mapped;
        }
        for (String tok : item.name().toLowerCase(Locale.ROOT).split("[^\\p{L}\\p{N}]+")) {
            String m = ALIASES.get(tok);
            if (m != null) {
                return m;
            }
        }
        return b.isEmpty() ? "?" : b;
    }
}
