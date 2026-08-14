package ru.tryberry.productmatch;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Доменная нормализация — то место, где живёт настоящая инженерия матчинга
 * (docs/PRODUCT-MATCH-JVM.md §9). Кейсы взяты не из головы: каждый уже ломал
 * питоновскую версию тех же правил в scripts/make-pairs.py.
 */
class NormalizationTest {

    private static Item item(String brand, String name) {
        return new Item("ozon", "1", brand, 0, name, "u");
    }

    @Test
    @DisplayName("объём памяти: все четыре написания площадок дают один ключ")
    void storageFormats() {
        // Ozon пишет через слэш, Я.Маркет вставляет единицу между числами и
        // иногда опускает её вовсе, WB пишет через плюс и в ОБРАТНОМ порядке.
        assertEquals("8/256", item("", "Смартфон 8/256 ГБ").storage());
        assertEquals("8/256", item("", "Смартфон 8 ГБ/256 ГБ").storage());
        assertEquals("8/256", item("", "Смартфон Camon 50 8/256").storage());
        assertEquals("8/256", item("", "Смартфон Note 15 Pro 256+8 ГБ").storage());
        assertEquals("12/512", item("", "Смартфон X8 Pro 12+512 Гб Черный").storage());
        // Пятый формат — через пробел, без разделителя. Найден на живой разметке:
        // подсказка врала «совпал» там, где 4/256 против 8/256.
        assertEquals("8/256", item("", "Смартфон Redmi 15C 8 256 Черный").storage());
        assertEquals("8/256", item("", "Смартфон Galaxy A17 8 256 ГБ (Черный)").storage());
        // Правдоподобие обязательно: «Note 15 8» не должно стать 15/8.
        assertEquals("?/256", item("", "Смартфон Note 15 256 ГБ").storage());
    }

    @Test
    @DisplayName("терабайты приводятся к гигабайтам: 1 ТБ и 1024 ГБ — одно и то же")
    void storageTerabytes() {
        assertEquals("?/1024", item("", "iPhone 15 Pro Max 1 ТБ").storage());
        assertEquals("?/1024", item("", "iPhone 15 Pro Max 1024 ГБ").storage());
        assertEquals("?/2048", item("", "iPhone 2TB").storage());
        assertEquals("16/1024", item("", "Смартфон 16/1 ТБ").storage());
    }

    @Test
    @DisplayName("Pro+ отличается от Pro, но 8+256 суффиксом не считается")
    void plusSuffix() {
        assertTrue(item("", "Redmi Note 15 Pro+ 5G 12/512").variantTokens().contains("pro+"));
        assertTrue(!item("", "Смартфон A17 8+256 ГБ").variantTokens().contains("+"));
        assertEquals(item("", "Смартфон A17 8+256 ГБ").variantTokens(), java.util.Set.of());
    }

    @Test
    @DisplayName("объём: одиночное число — это ПЗУ, оперативка неизвестна")
    void storageSingleNumber() {
        assertEquals("?/256", item("", "iPhone 14 Pro Max 256 ГБ").storage());
        assertEquals("", item("", "Смартфон без указания памяти").storage());
    }

    @Test
    @DisplayName("бренд: кириллица и латиница сводятся к одному ключу")
    void brandTransliteration() {
        assertEquals("xiaomi", Brands.normalize(item("Сяоми", "Смартфон Сяоми Redmi Note 13")));
        assertEquals("xiaomi", Brands.normalize(item("Xiaomi", "Xiaomi Смартфон Redmi Note 13")));
        // Суб-бренды сводятся к владельцу: на площадках они вперемешку.
        assertEquals("xiaomi", Brands.normalize(item("POCO", "Смартфон POCO X8 Pro")));
        assertEquals("samsung", Brands.normalize(item("", "Смартфон Galaxy A17 8/256 ГБ")));
    }

    @Test
    @DisplayName("бренд берётся из названия, если колонка пуста")
    void brandFromName() {
        assertEquals("apple", Brands.normalize(item("", "Смартфон Apple iPhone 15 128 ГБ")));
        assertEquals("?", Brands.normalize(item("", "Смартфон без бренда 128 ГБ")));
    }

    @Test
    @DisplayName("модельный код: артикул в скобках и латинский токен с цифрами")
    void modelTokens() {
        assertTrue(item("", "JURA E8 Piano black EC (15584)").modelTokens().contains("15584"));
        assertTrue(item("", "Робот-пылесос Midea VCR04W, 2000 мАч").modelTokens().contains("vcr04w"));
        assertTrue(item("", "vivo Смартфон Y31d Ростест (EAC) 8/128 ГБ").modelTokens().contains("y31d"));
    }

    @Test
    @DisplayName("квалификатор модели: Lite/Pro/Max/Note различают товары")
    void variantTokens() {
        assertTrue(item("", "HONOR 600 Lite Ростест 8/256").variantTokens().contains("lite"));
        assertTrue(item("", "Смартфон Magic 8 Pro Max").variantTokens().contains("max"));
        assertTrue(item("", "Xiaomi Redmi Note 13").variantTokens().contains("note"));
        // Цвет и слово «смартфон» квалификаторами не являются — это шум.
        assertTrue(item("", "Смартфон Galaxy A17 черный").variantTokens().isEmpty());
    }

    @Test
    @DisplayName("модельным кодом не считается слово без цифр и число без букв")
    void modelTokensNoise() {
        var tokens = item("", "Смартфон Apple iPhone 15 128 ГБ синий").modelTokens();
        assertTrue(tokens.stream().noneMatch(t -> t.equals("смартфон") || t.equals("синий")),
                "обычные слова не должны попадать в модельные коды: " + tokens);
    }
}
