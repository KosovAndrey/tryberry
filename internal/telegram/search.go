package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
)

// fsmTTL — сколько ждём ввод порога/процента, прежде чем состояние протухнет.
const fsmTTL = 10 * time.Minute

// searchFSM — состояние диалога поиск-подписки. Два режима:
//   - SellerURL задан → ждём слово-фильтр для витрины продавца (шаг до триггера);
//   - иначе (QueryID/Trigger) → ждём число (порог/процент) после выбора триггера.
type searchFSM struct {
	QueryID   int64  `json:"q"`
	Trigger   string `json:"t"`           // domain.TriggerBelowTarget | domain.TriggerDiscountPct
	SellerURL string `json:"u,omitempty"` // ждём текст-фильтр для этой витрины продавца
}

func fsmKey(tgID int64) string { return fmt.Sprintf("search_fsm:%d", tgID) }

func (b *Bot) getSearchFSM(ctx context.Context, tgID int64) (searchFSM, bool) {
	if b.rdb == nil {
		return searchFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, fsmKey(tgID)).Result()
	if err != nil {
		return searchFSM{}, false // redis.Nil или ошибка → состояния нет
	}
	var fsm searchFSM
	if err := json.Unmarshal([]byte(raw), &fsm); err != nil {
		return searchFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setSearchFSM(ctx context.Context, tgID int64, fsm searchFSM) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, fsmKey(tgID), raw, fsmTTL).Err()
}

func (b *Bot) clearSearchFSM(ctx context.Context, tgID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, fsmKey(tgID))
}

// isSearchURL — это поисковая ссылка поддерживаемого маркетплейса?
func (b *Bot) isSearchURL(text string) bool {
	_, err := b.registry.FindSearchByURL(text)
	return err == nil
}

// firstSearchURL возвращает поисковую ссылку из текста: сначала среди извлечённых
// http(s)-ссылок (сообщение может содержать текст вокруг), затем — весь текст,
// если он сам распознан как поисковая ссылка («голая» ссылка без схемы). "" если
// поисковой ссылки в тексте нет.
func (b *Bot) firstSearchURL(text string) string {
	for _, u := range bulkURLRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,);]")
		if b.isSearchURL(u) {
			return u
		}
	}
	if b.isSearchURL(text) {
		return text
	}
	return ""
}

// ── Меню «Поиск по ссылке» ────────────────────────────────────────────────────

func (b *Bot) sendSearchMenu(chatID int64, messageID int) {
	text := "🔎 <b>Поиск по ссылке</b>\n\n" +
		"Отправь ссылку на <b>поисковую выдачу</b> Wildberries, Яндекс.Маркета или Ozon прямо в чат — я буду следить за всей выдачей и пришлю, когда товары подешевеют.\n\n" +
		"Как получить ссылку: на сайте маркетплейса введи запрос в поиск, скопируй ссылку из адресной строки.\n\n" +
		"Примеры:\n" +
		"<code>https://www.wildberries.ru/catalog/0/search.aspx?search=наушники</code>\n" +
		"<code>https://market.yandex.ru/search?text=наушники</code>\n" +
		"<code>https://www.ozon.ru/search/?text=наушники</code>\n\n" +
		"🏬 <b>Магазин на WB.</b> Можно прислать ссылку на витрину продавца " +
		"(<code>wildberries.ru/seller/…</code>) — слежу за ценами всего магазина. " +
		"У больших магазинов сузь выдачу фильтрами на сайте (категория, бренд), иначе товаров будет слишком много.\n\n" +
		"Совет: чем точнее запрос, тем меньше лишнего в уведомлениях."

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои поиск-подписки", "menu:lsearch"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Назад", "menu:main"),
		),
	)
	b.editMenu(chatID, messageID, text, keyboard)
}

// ── Старт подписки: ссылка → выбор типа триггера ──────────────────────────────

func (b *Bot) startSearchTrack(ctx context.Context, chatID int64, rawURL string, user *domain.User) {
	// Фейл-фаст: если поиск на тарифе недоступен или лимит исчерпан —
	// не показываем кнопки, сразу объясняем.
	plan := user.EffectivePlan(time.Now())
	cnt, err := b.searchSubRepo.CountActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("count search subs", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}
	if cnt >= plan.MaxSearch {
		b.reply(chatID, b.searchLimitText(plan, cnt))
		return
	}

	ss, err := b.registry.FindSearchByURL(rawURL)
	if err != nil {
		b.reply(chatID, "Это не похоже на поисковую ссылку. Нужна ссылка на поисковую выдачу (Wildberries, Яндекс.Маркет или Ozon) с текстом запроса.")
		return
	}

	// Гейт CAP для витрины продавца: если товаров в выдаче больше лимита —
	// подписку не заводим, просим сузить фильтры. Для не-seller-скрейперов no-op.
	if !b.checkSellerCap(ctx, chatID, ss, rawURL) {
		return
	}

	// Витрина продавца без явного текст-фильтра → предложить добавить его, чтобы
	// следить не за всем магазином, а за частью карточек (по слову в названии).
	if _, isSeller := ss.(sellerSizer); isSeller && !domain.HasTextFilter(rawURL) {
		b.promptSellerTextFilter(ctx, chatID, user, rawURL)
		return
	}

	b.proceedSearchTrack(ctx, chatID, rawURL, user)
}

// proceedSearchTrack — финал подключения поиск-подписки: нормализуем URL,
// заводим/находим запрос и показываем выбор типа триггера. Гейт лимита/CAP уже
// пройден в startSearchTrack.
func (b *Bot) proceedSearchTrack(ctx context.Context, chatID int64, rawURL string, user *domain.User) {
	ss, err := b.registry.FindSearchByURL(rawURL)
	if err != nil {
		b.reply(chatID, "Это не похоже на поисковую ссылку. Нужна ссылка на поисковую выдачу (Wildberries, Яндекс.Маркет или Ozon) с текстом запроса.")
		return
	}
	normalized, err := ss.NormalizeSearchURL(rawURL)
	if err != nil {
		b.reply(chatID, "Не получилось разобрать поисковый запрос из ссылки. Проверь, что в ней есть текст поиска.")
		return
	}

	// Для витрины продавца — человекочитаемый ярлык с именем магазина (вместо
	// «Магазин #{id}» из URL). Имя берём из supplier-by-id; сбой → URL-фолбэк.
	queryText := domain.QueryTextFromNormalized(normalized)
	if n, ok := ss.(sellerNamer); ok {
		if name, err := n.SellerName(ctx, rawURL); err == nil && name != "" {
			queryText = domain.SellerLabel(name, rawURL)
		}
	}

	sq, _, err := b.searchQueryRepo.Upsert(ctx, string(ss.Marketplace()), normalized, queryText, nil)
	if err != nil {
		b.log.Error("upsert search query", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	text := fmt.Sprintf("🔎 Запрос: <b>%s</b>\n\n", htmlEscape(sq.QueryText))
	// Подсказка про фильтры для обычного поиска (не витрина — у неё свой шаг) и
	// только если фильтра ещё нет: широкий запрос даёт много разных товаров.
	// Совет ненавязчивый, не обесцениваем (следим за всей выдачей), даём явный
	// выход. Слежение стартует только после выбора стратегии.
	isSeller := strings.Contains(normalized, "/seller/") || strings.Contains(normalized, "business--")
	if !isSeller && !domain.HasTextFilter(rawURL) && !domain.SearchHasSiteFilter(rawURL) {
		text += "💡 Запрос без фильтров — под него подходит очень много разных товаров. " +
			"Чтобы получать уведомления только о том, что нужно именно тебе, сузь выдачу на сайте " +
			"(категория, бренд, цена) и пришли новую ссылку.\n\n" +
			"Если подборка на маркетплейсе тебя устраивает — выбери ниже, как уведомлять, и я начну следить 👇"
	} else {
		text += "Как уведомлять о снижении цены?"
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📉 Ниже цены", fmt.Sprintf("strack:%d:below", sq.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔻 Любое снижение", fmt.Sprintf("strack:%d:any", sq.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("％ Скидка от %", fmt.Sprintf("strack:%d:disc", sq.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Отменить", "scancel"),
		),
	)
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
}

// promptSellerTextFilter — спросить опциональный текст-фильтр для витрины
// продавца. Ставит FSM (ждём слово) и показывает кнопку «Без фильтра».
func (b *Bot) promptSellerTextFilter(ctx context.Context, chatID int64, user *domain.User, rawURL string) {
	if err := b.setSearchFSM(ctx, user.TelegramID, searchFSM{SellerURL: rawURL}); err != nil {
		// Без Redis шаг недоступен — не теряем подключение, идём без фильтра.
		b.log.Error("set seller text fsm", "err", err)
		b.proceedSearchTrack(ctx, chatID, rawURL, user)
		return
	}
	text := "🏬 <b>Магазин распознан.</b>\n\n" +
		"Следить за всеми товарами магазина или только за частью? Пришли слово — оставлю карточки, " +
		"в названии которых оно есть (например <code>iphone 17</code>).\n\n" +
		"Или нажми «Без фильтра», чтобы следить за всей выдачей."
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏭ Без фильтра", "sfskip"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Отменить", "scancel"),
		),
	)
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
}

// handleSellerTextFilter — пользователь прислал слово-фильтр для витрины продавца
// (FSM в режиме SellerURL). Дописываем tb_q и идём к выбору триггера.
func (b *Bot) handleSellerTextFilter(ctx context.Context, chatID, tgID int64, text string, user *domain.User, fsm searchFSM) {
	b.clearSearchFSM(ctx, tgID)

	// Прислали новую ссылку вместо слова → начинаем флоу заново по ней.
	if b.isSearchURL(text) {
		b.startSearchTrack(ctx, chatID, text, user)
		return
	}

	rawURL := fsm.SellerURL
	if t := strings.TrimSpace(text); t != "" {
		rawURL = domain.AppendTextFilter(rawURL, t)
	}
	b.proceedSearchTrack(ctx, chatID, rawURL, user)
}

// handleSellerSkipFilter — кнопка «Без фильтра»: подключаем магазин целиком.
func (b *Bot) handleSellerSkipFilter(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	fsm, ok := b.getSearchFSM(ctx, cb.From.ID)
	if !ok || fsm.SellerURL == "" {
		b.answerCallback(cb.ID, "Это действие уже неактуально")
		return
	}
	b.clearSearchFSM(ctx, cb.From.ID)
	user, err := b.userRepo.Upsert(ctx, cb.From.ID, cb.From.UserName)
	if err != nil {
		b.log.Error("upsert user", "err", err)
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	b.answerCallback(cb.ID, "")
	b.proceedSearchTrack(ctx, cb.Message.Chat.ID, fsm.SellerURL, user)
}

// sellerSizer — скрейпер витрины продавца: умеет узнать размер выдачи и свой
// потолок (CAP). Реализует *scraper.WildberriesSellerScraper.
type sellerSizer interface {
	SellerTotal(ctx context.Context, rawURL string) (int, error)
	SellerName(ctx context.Context, rawURL string) (string, error)
	MaxItems() int
}

// sellerNamer — витрина, умеющая отдать имя магазина для ярлыка (без CAP-гейта).
// Удовлетворяют и WB-seller (через trademark), и YM-витрина (через слаг ссылки).
type sellerNamer interface {
	SellerName(ctx context.Context, rawURL string) (string, error)
}

// checkSellerCap — гейт размера витрины продавца. Возвращает false (и отвечает
// юзеру), если магазин пуст или товаров больше CAP. Для обычных поиск-ссылок —
// no-op (true). Сбой запроса размера не блокирует: пропускаем (true).
func (b *Bot) checkSellerCap(ctx context.Context, chatID int64, ss interface{}, rawURL string) bool {
	sz, ok := ss.(sellerSizer)
	if !ok {
		return true // не витрина продавца — гейт не нужен
	}
	total, err := sz.SellerTotal(ctx, rawURL)
	if err != nil {
		b.log.Error("seller total", "err", err)
		return true // best-effort: не валим подключение из-за сбоя проверки
	}
	if total == 0 {
		b.reply(chatID, "🏬 В этом магазине по такой ссылке нет товаров. Проверь ссылку или ослабь фильтры.")
		return false
	}
	if limit := sz.MaxItems(); total > limit {
		b.reply(chatID, fmt.Sprintf(
			"🏬 В выдаче <b>%d</b> товаров — это больше лимита (<b>%d</b>).\n\n"+
				"Сузь выбор фильтрами на сайте (категория, бренд, модель) и пришли ссылку снова. "+
				"Можно также добавить в конец ссылки <code>&tb_q=текст</code> — я оставлю только карточки с этим текстом в названии.",
			total, limit))
		return false
	}
	return true
}

// handleSearchCancel — отмена на шаге выбора стратегии (кнопка «❌ Отменить»).
// Подписка ещё не создана (есть только общая строка запроса), чистить нечего —
// просто закрываем сообщение.
func (b *Bot) handleSearchCancel(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	b.clearSearchFSM(ctx, cb.From.ID) // на случай отмены с шага фильтра витрины
	b.answerCallback(cb.ID, "Отменено")
	edit := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID,
		"❌ Отменено. Пришли другую ссылку, когда будешь готов.")
	if _, err := b.api.Send(edit); err != nil {
		b.log.Error("edit search cancel", "err", err)
	}
}

// ── Выбор типа триггера (callback strack:<qid>:<type>) ────────────────────────

func (b *Bot) handleSearchTriggerCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	parts := strings.Split(strings.TrimPrefix(cb.Data, "strack:"), ":")
	if len(parts) != 2 {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	queryID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	kind := parts[1]
	chatID := cb.Message.Chat.ID

	user, err := b.userRepo.Upsert(ctx, cb.From.ID, cb.From.UserName)
	if err != nil {
		b.log.Error("upsert user", "err", err)
		b.answerCallback(cb.ID, "Ошибка")
		return
	}

	switch kind {
	case "any":
		b.createSearchSub(ctx, chatID, user, queryID, domain.TriggerAnyDrop, nil, nil)
	case "below":
		if err := b.setSearchFSM(ctx, cb.From.ID, searchFSM{QueryID: queryID, Trigger: string(domain.TriggerBelowTarget)}); err != nil {
			b.reply(chatID, "Не получилось начать ввод (нет связи с хранилищем). Попробуй «Любое снижение».")
			return
		}
		b.reply(chatID, "💰 Введи целевую цену в рублях (например <code>59990</code>).\nУведомлю, когда найдётся товар дешевле.")
	case "disc":
		if err := b.setSearchFSM(ctx, cb.From.ID, searchFSM{QueryID: queryID, Trigger: string(domain.TriggerDiscountPct)}); err != nil {
			b.reply(chatID, "Не получилось начать ввод (нет связи с хранилищем). Попробуй «Любое снижение».")
			return
		}
		b.reply(chatID, "％ Введи процент скидки от стартовой цены (1–99, например <code>20</code>).")
	default:
		b.answerCallback(cb.ID, "Ошибка")
	}
}

// ── Ввод числа (порог/процент) ────────────────────────────────────────────────

func (b *Bot) handleSearchThreshold(ctx context.Context, chatID, tgID int64, text string, user *domain.User, fsm searchFSM) {
	switch domain.TriggerType(fsm.Trigger) {
	case domain.TriggerBelowTarget:
		price, err := domain.ParsePrice(text)
		if err != nil {
			b.reply(chatID, "Нужно число — цена в рублях, например <code>59990</code>. Или /menu чтобы отменить.")
			return
		}
		b.clearSearchFSM(ctx, tgID)
		b.createSearchSub(ctx, chatID, user, fsm.QueryID, domain.TriggerBelowTarget, &price, nil)

	case domain.TriggerDiscountPct:
		pct, err := domain.ParsePct(text)
		if err != nil {
			b.reply(chatID, "Нужно целое число от 1 до 99, например <code>20</code>. Или /menu чтобы отменить.")
			return
		}
		b.clearSearchFSM(ctx, tgID)
		b.createSearchSub(ctx, chatID, user, fsm.QueryID, domain.TriggerDiscountPct, nil, &pct)

	default:
		b.clearSearchFSM(ctx, tgID)
		b.reply(chatID, "Что-то пошло не так, начни заново через /menu.")
	}
}

// createSearchSub — создать поиск-подписку и зафиксировать стартовые цены.
func (b *Bot) createSearchSub(ctx context.Context, chatID int64, user *domain.User, queryID int64, trigger domain.TriggerType, target *float64, pct *int16) {
	// Жёсткий guard на случай гонки/обхода фейл-фаста.
	plan := user.EffectivePlan(time.Now())
	if cnt, err := b.searchSubRepo.CountActiveByUserID(ctx, user.ID); err == nil && cnt >= plan.MaxSearch {
		b.reply(chatID, b.searchLimitText(plan, cnt))
		return
	}

	sub, err := b.searchSubRepo.Create(ctx, &domain.SearchSubscription{
		UserID:        user.ID,
		SearchQueryID: queryID,
		TriggerType:   trigger,
		TargetPrice:   target,
		DiscountPct:   pct,
	})
	if err != nil {
		b.log.Error("create search subscription", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	// Зафиксировать baseline по уже известным товарам выдачи (если запрос
	// скрейпился раньше). Для нового запроса выдача пуста — baseline заведёт
	// планировщик при первом скрейпе.
	if _, err := b.searchSubRepo.BackfillBaselines(ctx, sub.ID, queryID); err != nil {
		b.log.Warn("backfill baselines", "sub_id", sub.ID, "err", err)
	}

	// Мгновенная первая оценка below_target по сохранённой выдаче: иначе
	// подписка, созданная между скрейпами, ждёт следующего (Ozon-пол 15м,
	// free 6ч) при уже лежащих в БД подходящих товарах. См. searchsub.instant.
	if b.searchResults != nil && b.searchEvents != nil {
		if q, err := b.searchQueryRepo.GetByID(ctx, queryID); err == nil {
			n, err := searchsub.SendInstantBelowTarget(ctx, b.searchResults, b.searchSubRepo, b.searchEvents, sub, q, user.TelegramID)
			if err != nil {
				b.log.Warn("instant search eval", "sub_id", sub.ID, "err", err)
			} else if n > 0 {
				b.log.Info("instant search hits queued", "sub_id", sub.ID, "items", n)
			}
		}
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои поиск-подписки", "menu:lsearch"),
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)
	m := tgbotapi.NewMessage(chatID, "✅ <b>Готово!</b> "+domain.TriggerDescription(trigger, target, pct)+"\n\nПроверяю выдачу регулярно и пришлю, когда товары подешевеют 🔔")
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
}

// ── Список поиск-подписок ─────────────────────────────────────────────────────

func (b *Bot) handleListSearch(ctx context.Context, chatID int64, user *domain.User) {
	subs, err := b.searchSubRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("get search subscriptions", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}
	if len(subs) == 0 {
		m := tgbotapi.NewMessage(chatID,
			"🔎 У тебя пока нет поиск-подписок.\n\nОтправь ссылку на поисковую выдачу Wildberries, Яндекс.Маркета или Ozon прямо в чат.")
		m.ParseMode = "HTML"
		m.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
			),
		)
		b.send(m)
		return
	}
	text, keyboard := b.buildSearchListView(subs)
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
}

func (b *Bot) buildSearchListView(subs []*domain.SearchSubscription) (string, tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔎 <b>Поиск-подписки — %d активных</b>\n\n", len(subs))
	for i, s := range subs {
		fmt.Fprintf(&sb, "%d. %s <b>%s</b>\n   %s\n\n",
			i+1, marketplaceIcon(s.Marketplace), htmlEscape(s.QueryText),
			domain.TriggerDescription(s.TriggerType, s.TargetPrice, s.DiscountPct))
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, s := range subs {
		cancel := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("❌ Отменить #%d", i+1),
			fmt.Sprintf("suntrack:%d", s.ID),
		)
		// Битый URL не должен ронять всю клавиатуру (см. safeButtonURL).
		if u := safeButtonURL(s.NormalizedURL); u != "" {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonURL(fmt.Sprintf("🔗 #%d %s", i+1, truncate(s.QueryText, 18)), u),
				cancel,
			))
		} else {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(cancel))
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))
	return sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) callbackUntrackSearch(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	id, err := strconv.ParseInt(strings.TrimPrefix(cb.Data, "suntrack:"), 10, 64)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}

	// id — из callback_data; гасим только если подписка принадлежит этому юзеру.
	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	if err := b.searchSubRepo.Deactivate(ctx, id, user.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("deactivate search sub", "err", err)
		b.answerCallback(cb.ID, "Ошибка, попробуй позже")
		return
	}

	subs, err := b.searchSubRepo.GetActiveByUserID(ctx, user.ID)
	if err == nil && len(subs) > 0 {
		text, keyboard := b.buildSearchListView(subs)
		b.editMenu(cb.Message.Chat.ID, cb.Message.MessageID, text, keyboard)
		b.answerCallback(cb.ID, "✅ Поиск-подписка отменена")
		return
	}
	b.sendMainMenu(ctx, cb.Message.Chat.ID, cb.Message.MessageID, true)
	b.answerCallback(cb.ID, "✅ Поиск-подписка отменена")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// ── Тексты отказов по лимитам тарифа ─────────────────────────────────────────

func productLimitText(plan domain.Plan, used int) string {
	return fmt.Sprintf(
		"🚫 Достигнут лимит тарифа <b>%s</b>: товаров %d из %d.\n\n"+
			"Чтобы отслеживать больше — оформи тариф повыше (/plans) или отмени ненужное в /list.",
		plan.Title, used, plan.MaxProduct)
}

func (b *Bot) searchLimitText(plan domain.Plan, used int) string {
	if plan.MaxSearch == 0 {
		return "🔎 Поиск-подписки на твоём тарифе пока недоступны.\n\n" +
			"Поиск по ссылке есть на тарифах <b>Lite</b> и выше — /plans. Также можно попробовать бесплатный триал на 10 дней — команда /trial.\n\n" +
			"По вопросам — @kosov_andrey."
	}
	if plan.Name == "free" {
		metrics.SearchUpsellShown.WithLabelValues("tg").Inc()
		return fmt.Sprintf(
			"🚫 На тарифе <b>Free</b> доступна одна поиск-подписка (%d из %d занято), проверка — %s.\n\n"+
				"Больше поисков и проверка чаще — на тарифах <b>Lite</b> и <b>Pro</b>: /plans. Новым пользователям — бесплатный триал на 10 дней: /trial.\n\n"+
				"Отменить ненужное: /list_search.",
			used, plan.MaxSearch, domain.IntervalPhrase(plan.EffectiveSearchInterval(plan.Interval)))
	}
	return fmt.Sprintf(
		"🚫 Достигнут лимит поиск-подписок тарифа <b>%s</b>: %d из %d.\n\n"+
			"Оформи тариф повыше (/plans) или отмени ненужное в /list_search.",
		plan.Title, used, plan.MaxSearch)
}
