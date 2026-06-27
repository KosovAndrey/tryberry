---
name: vk-tg-parity
description: VK-бот доведён до полного функционального паритета с Telegram (ветка feat/vk-chart-links)
metadata:
  type: project
---

Требование пользователя: «VK должен быть ровно таким же, как TG — полный функционал».
Закрыто на ветке `feat/vk-chart-links` (2026-06-26), 4 коммита:

1. Ссылка «📈 График цены» в VK — трек/`/list`/пуши (см. [[price-charts-web]]).
2. Балк-трек (`internal/vk/bulktrack.go`) — 2+ ссылок одним сообщением → сводка.
3. Полный паритет (`internal/vk/admin.go`): админ-команды `/grant /revoke /extend
   /users /whois /promo_create /promo_off /promo_list` (по `VK_ADMIN_IDS`, целевой
   юзер по telegram_id), OOS-флоу одиночного трека (back_in_stock + vkTrackOOSKeyboard),
   `/myplan`, роутер слэш-команд `handleSlashCommand` (все команды TG + алиасы).
4. Подсказки целевой цены в «ниже цены» (honest-price SuggestTargets, payload `ptgt`)
   — пробросил `priceRepo` в `vk.Bot`.

Что осталось НАМЕРЕННО различным (не пробелы): TG редактирует сообщение (editMenu),
VK шлёт новое (API VK); `/list` в VK даёт график текст-ссылкой, а не inline-кнопкой
(лимит 10 кнопок); у VK есть ЛИШНЕЕ — привязка/отвязка Telegram-аккаунта. Админка
VK keyed по `VK_ADMIN_IDS` (env проброшен в docker-compose, пусто = админа в VK нет;
см. [[admin-commands]]).

Деплой: пересобрать `bot-worker` (+`notifier` ради ссылки графика в VK-пуше). Тестов
в пакете `vk` нет (как и было) — ядро зеркалит покрытый TG.
