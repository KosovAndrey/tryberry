# Ранбук: «Обманчивый сайт» в Safari на iOS

Дата: 2026-08-29. Симптом: Safari на iPhone показывает красный экран «Safari
обнаружил, что сайт tryberry.ru может быть мошенническим». Воспроизводится на
разных айфонах в разных сетях.

## Вердикт: это Apple, а не Google

Safari отправляет проверку **в два места — в Google Safe Browsing И в Apple**.
У нас Google чист по трём независимым источникам, значит запись держит Apple.

| Источник | Результат |
|---|---|
| Search Console (домен подтверждён по DNS) → Проблемы безопасности | «Проблем не обнаружено» |
| Google Transparency Report (см. способ ниже) | код 1, все флаги угроз false |
| **Chrome на том же iPhone** | открывает без предупреждения |
| Chrome на Android | открывает без предупреждения |
| Safari на iPhone | предупреждение |

Решающий тест — **выключить Настройки → Safari → «Предупреждение о мошенническом
сайте»**: предупреждение исчезает, включить обратно — появляется. Значит это
именно Fraudulent Website Warning, а не контент-блокировщик, не DNS-фильтр и не
Private Relay (те работали бы и в Chrome, и на Android).

### Как проверить статус в Google без Search Console

Внутренний API Transparency Report, отвечает обычным curl:

```bash
curl -s "https://transparencyreport.google.com/transparencyreport/api/v3/safebrowsing/status?site=tryberry.ru" \
  -H "User-Agent: Mozilla/5.0" | tail -1
```

Ответ: `[["sb.ssr",<код>,<флаги угроз>,...,<timestamp>,"<домен>",false]]`.
Коды, откалиброванные по заведомо известным доменам:

| Код | Что значит | Эталон |
|---|---|---|
| 3 | опасный (рядом стоят `true` в флагах) | `testsafebrowsing.appspot.com`, `malware.testing.google.test` |
| 4 | проверен, безопасен | wikipedia.org, github.com, google.com |
| 1 | нет данных о небезопасности (обычное состояние малоизвестного домена) | example.com, **tryberry.ru** |
| 6 | вообще нет записи | домен, который Google не смотрел |

## Что уже исправлено (2026-08-29)

Не потому, что доказана вина, а потому что это в любом случае мины:

- удалены ~30 публичных страниц `/screen*`, `/wall*` — заготовки для съёмки
  промо, копировавшие интерфейс мессенджера (шапка чата с галочкой, поле ввода,
  кнопка отправки); у `/screenapp` был standalone-манифест, на iOS страница
  открывалась полноэкранно, без адресной строки. Теперь 410;
- закрыт `web/CLAUDE.md` — внутренняя инструкция отдавалась публично, 200.

Что осталось и трогать не надо: упоминания маркетплейсов (в футере есть
дисклеймер «не является продавцом, представителем, партнёром или официальным
сервисом»), приём платежей (оферта и политика опубликованы, форм ввода на сайте
нет вообще).

## Что проверено и исключено

| Гипотеза | Как проверена | Итог |
|---|---|---|
| Сертификат / кто его подписал | `openssl s_client`: полная цепочка Let's Encrypt до ISRG Root X2, `Verification: OK` | ❌ |
| Хостинг, IP, регистратор, **зона .ru** | `botyanit.ru` — тот же владелец, тот же reg.ru, **тот же IP 194.164.245.150**, тот же nginx, тот же Let's Encrypt — предупреждения нет | ❌ |
| Сеть, оператор, DNS-фильтр, VPN | Chrome на **том же** iPhone в той же сети — чисто | ❌ |
| Google Safe Browsing | три источника выше | ❌ |
| Соседи по имени (`tryberry.com` продаётся на HugeDomains) | статус `.com`, `.org`, `.app` — все код 1, ни один не помечен; репутация между зонами не переносится, списки адресуют конкретный домен | ❌ |
| Контент-блокировщик Safari | предупреждение управляется тумблером Fraudulent Website Warning | ❌ |

Остался один кандидат: **запись в списке Apple**.

## Что делать

### 1. Письмо в Apple (главное действие)

Формы у Apple нет, есть адрес **websitereview@apple.com**. Отвечают медленно и
не всегда, но это единственный официальный канал. Шаблон письма — в конце файла.

### 2. Продублировать через Google

<https://safebrowsing.google.com/safebrowsing/report_error/> — на случай, если
запись всё же где-то в цепочке GSB и просто не видна в Transparency Report.
Стоит ноль, отправляется за минуту.

### 3. Пока ждём — вести ролики в мессенджеры, не на сайт

`t.me/TryBerryBot`, `vk.me/tryberrybot`, `max.ru/se13426918_bot` — вердикт на них
не распространяется, а метку атрибуции они несут в deep-link сами. Сайт в
эндкарде оставить можно, но единственной дверью он быть не должен.

## Про второй домен

`tryberry.online` куплен (2026-08-29), конфиг готов —
`nginx/conf.d/tryberry-online.conf` + `-ssl.conf.tpl`. Но помнить два факта:

- **Смена домена не лечит, а обходит.** Причина, если она в контенте или
  репутации, переедет вместе с сайтом, просто позже.
- **Зона `.online` по репутации хуже, чем `.ru`.** Дешёвые новые gTLD
  (`.online`, `.top`, `.xyz`) исторически используются в спаме и фишинге, и
  базовый риск-скор у них выше. Если бояться строгости фильтров, `.online` —
  худший выбор из имеющихся, а не лучший.
- Версия «нероссийский домен строгим фильтрам нравится больше» **опровергнута
  собственным экспериментом**: `botyanit.ru` в той же зоне `.ru`, у того же
  регистратора, на том же IP — предупреждения нет. Зона ни при чём.

Поэтому покупать `tryberry.com` за $3400 или `.io` под эту задачу не нужно:
проблема не в имени домена. (И если reg.ru показывает `.com` за 1700 ₽ в
**месяц** — это не цена регистрации, обычный `.com` стоит порядка 1000–2000 ₽
в **год**; стоит перепроверить, что именно в корзине.)

## Как понять, что сняли

- на iPhone открыть в **приватной** вкладке (обычная показывает кэш);
- локальный список в Safari обновляется не мгновенно — до суток;
- проверить на втором устройстве в другой сети.

## Шаблон письма в Apple

```
Subject: False positive Deceptive Website Warning — tryberry.ru

Hello,

Safari on iOS shows a "Deceptive Website Warning" for https://tryberry.ru/,
reproduced on multiple iPhones on different networks. Chrome on the same
iPhone and on Android opens the site without any warning. Turning off
Settings > Safari > Fraudulent Website Warning removes the warning, which
points to the Safe Browsing list rather than to the network or the device.

Google Safe Browsing does not flag this domain:
- Search Console (verified owner, domain property): Security Issues —
  "No issues detected"
- Transparency Report: no unsafe content reported for tryberry.ru

tryberry.ru is a price-tracking service for online marketplaces. The site does
not collect credentials or payment data — there are no input forms on the site
at all. It does not impersonate any brand: the footer explicitly states the
service is not a seller, representative, partner or official service of
Wildberries, Ozon, Yandex Market or AliExpress, and publishes the operator's
legal details and terms of service.

We have also removed internal staging pages (/screen*, /wall*) that were used
for recording promotional videos and visually resembled a messenger interface.
They now return HTTP 410.

Could you please review the domain and remove the warning?

Thank you,
Andrey Kosov
```
