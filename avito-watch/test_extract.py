"""Проверки разбора выдачи: python3 -m unittest test_extract (без браузера)."""

import html
import json
import unittest

import extract

RAW = {
    "id": 4567890123,
    "title": "2-к. квартира, 54 м², 7/12 эт.",
    "urlPath": "/moskva/kvartiry/2-k._kvartira_54m_712et._4567890123",
    "priceDetailed": {"value": 65000, "fullString": "65 000 ₽ в месяц"},
    "sortTimeStamp": 1789640000000,
    "geo": {"formattedAddress": "ул. Пример, 5",
            "geoReferences": [{"content": "Сокол", "after": "6–10 мин."}]},
    "images": [{"208x156": "https://img/small.jpg", "864x648": "https://img/big.jpg"}],
    "iva": {
        "DateInfoStep": [{"componentData": {"component": "text", "payload": {"text": "2 часа назад"}}}],
        "UserInfoStep": [{"componentData": {"component": "seller", "payload": {"name": "Анна", "text": "Собственник"}}},
                         {"componentData": {"component": "rating", "payload": {"score": "4,9", "summary": "12 отзывов"}}}],
    },
    "description": "Сдаю на длительный срок",
}


def page_script(state: dict) -> str:
    return html.escape(json.dumps(state, ensure_ascii=False))


class ExtractTest(unittest.TestCase):
    def test_state_items_normalized(self):
        state = {"i18n": {"hasMessages": True},
                 "loaderData": {"data": {"catalog": {"items": [RAW, {"type": "banner"}]}}}}
        raw = extract.raw_items_from_state(["{broken", page_script(state)])
        self.assertEqual(len(raw), 1)
        it = extract.normalize(raw[0])
        self.assertEqual(it["id"], "4567890123")
        self.assertEqual(it["price"], 65000)
        self.assertEqual(it["url"], "https://www.avito.ru" + RAW["urlPath"])
        self.assertEqual(it["photo"], "https://img/big.jpg")
        self.assertEqual(it["metro"], ["Сокол 6–10 мин."])
        self.assertEqual(it["seller"], ["Анна", "Собственник", "4,9", "12 отзывов"])

    def test_empty_catalog_is_not_missing_state(self):
        state = {"loaderData": {"data": {"catalog": {"items": []}}}}
        self.assertEqual(extract.raw_items_from_state([page_script(state)]), [])
        self.assertIsNone(extract.raw_items_from_state([page_script({"x": 1})]))

    def test_normalize_search_url(self):
        u = extract.normalize_search_url("m.avito.ru/moskva/kvartiry/sdam?p=3&s=1&f=ASg&context=abc")
        self.assertEqual(u, "https://www.avito.ru/moskva/kvartiry/sdam?f=ASg&s=104")
        self.assertIsNone(extract.normalize_search_url("https://ozon.ru/search?text=x"))

    def test_blocked(self):
        self.assertTrue(extract.is_blocked("Доступ ограничен: проблема с IP", "", 200))
        self.assertTrue(extract.is_blocked("Авито", "", 429))
        self.assertFalse(extract.is_blocked("Снять квартиру в Москве", "объявления", 200))


if __name__ == "__main__":
    unittest.main()
