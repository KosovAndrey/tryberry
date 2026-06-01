package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// fsmTTL — сколько ждём ввод порога/процента, прежде чем состояние протухнет.
const fsmTTL = 10 * time.Minute

// searchFSM — минимальное состояние диалога: ждём число для подписки.
type searchFSM struct {
	QueryID int64  `json:"q"`
	Trigger string `json:"t"` // domain.TriggerBelowTarget | domain.TriggerDiscountPct
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

// ── Меню «Поиск по ссылке» ────────────────────────────────────────────────────

func (b *Bot) sendSearchMenu(chatID int64, messageID int) {
	text := "🔎 <b>Поиск по ссылке</b>\n\n" +
		"Отправь ссылку на <b>поисковую выдачу</b> Wildberries прямо в чат — я буду следить за всей выдачей и пришлю, когда товары подешевеют.\n\n" +
		"Как получить ссылку: на сайте WB введи запрос в поиск, скопируй ссылку из адресной строки.\n\n" +
		"Пример:\n<code>https://www.wildberries.ru/catalog/0/search.aspx?search=наушники</code>\n\n" +
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

func (b *Bot) startSearchTrack(ctx context.Context, chatID int64, rawURL string, _ *domain.User) {
	ss, err := b.registry.FindSearchByURL(rawURL)
	if err != nil {
		b.reply(chatID, "Это не похоже на поисковую ссылку Wildberries. Нужна ссылка с параметром поиска.")
		return
	}

	normalized, err := ss.NormalizeSearchURL(rawURL)
	if err != nil {
		b.reply(chatID, "Не получилось разобрать поисковый запрос из ссылки. Проверь, что в ней есть текст поиска.")
		return
	}

	sq, _, err := b.searchQueryRepo.Upsert(ctx, string(ss.Marketplace()), normalized, queryTextFromNormalized(normalized), nil)
	if err != nil {
		b.log.Error("upsert search query", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	text := fmt.Sprintf(
		"🔎 Запрос: <b>%s</b>\n\nКак уведомлять о снижении цены?",
		htmlEscape(sq.QueryText),
	)
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
	)
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
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
		price, err := parsePrice(text)
		if err != nil {
			b.reply(chatID, "Нужно число — цена в рублях, например <code>59990</code>. Или /menu чтобы отменить.")
			return
		}
		b.clearSearchFSM(ctx, tgID)
		b.createSearchSub(ctx, chatID, user, fsm.QueryID, domain.TriggerBelowTarget, &price, nil)

	case domain.TriggerDiscountPct:
		pct, err := parsePct(text)
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

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои поиск-подписки", "menu:lsearch"),
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)
	m := tgbotapi.NewMessage(chatID, "✅ <b>Готово!</b> "+triggerDescription(trigger, target, pct)+"\n\nПроверяю выдачу регулярно и пришлю, когда товары подешевеют 🔔")
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
			"🔎 У тебя пока нет поиск-подписок.\n\nОтправь ссылку на поисковую выдачу Wildberries прямо в чат.")
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
		fmt.Fprintf(&sb, "%d. <b>%s</b>\n   %s\n\n",
			i+1, htmlEscape(s.QueryText), triggerDescription(s.TriggerType, s.TargetPrice, s.DiscountPct))
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, s := range subs {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL(
				fmt.Sprintf("🔗 #%d %s", i+1, truncate(s.QueryText, 18)),
				s.NormalizedURL,
			),
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("❌ Отменить #%d", i+1),
				fmt.Sprintf("suntrack:%d", s.ID),
			),
		))
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
	if err := b.searchSubRepo.Deactivate(ctx, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("deactivate search sub", "err", err)
		b.answerCallback(cb.ID, "Ошибка, попробуй позже")
		return
	}

	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err == nil {
		subs, err := b.searchSubRepo.GetActiveByUserID(ctx, user.ID)
		if err == nil && len(subs) > 0 {
			text, keyboard := b.buildSearchListView(subs)
			b.editMenu(cb.Message.Chat.ID, cb.Message.MessageID, text, keyboard)
			b.answerCallback(cb.ID, "✅ Поиск-подписка отменена")
			return
		}
	}
	b.sendMainMenu(ctx, cb.Message.Chat.ID, cb.Message.MessageID, true)
	b.answerCallback(cb.ID, "✅ Поиск-подписка отменена")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// queryTextFromNormalized — достать человекочитаемый запрос из нормализованного
// URL (...search.aspx?search=...&sort=...) для отображения.
func queryTextFromNormalized(normalized string) string {
	const marker = "search="
	i := strings.Index(normalized, marker)
	if i < 0 {
		return normalized
	}
	rest := normalized[i+len(marker):]
	if amp := strings.IndexByte(rest, '&'); amp >= 0 {
		rest = rest[:amp]
	}
	rest = strings.ReplaceAll(rest, "+", " ")
	if dec, err := url.QueryUnescape(rest); err == nil {
		return dec
	}
	return rest
}

func triggerDescription(t domain.TriggerType, target *float64, pct *int16) string {
	switch t {
	case domain.TriggerBelowTarget:
		if target != nil {
			return fmt.Sprintf("📉 уведомлю, когда цена опустится ниже %.0f ₽", *target)
		}
		return "📉 уведомлю при достижении целевой цены"
	case domain.TriggerAnyDrop:
		return "🔻 уведомлю при любом снижении цены"
	case domain.TriggerDiscountPct:
		if pct != nil {
			return fmt.Sprintf("％ уведомлю при скидке от %d%%", *pct)
		}
		return "％ уведомлю при заметной скидке"
	default:
		return ""
	}
}

func parsePrice(s string) (float64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.TrimSuffix(s, "₽")
	s = strings.TrimSuffix(s, "руб")
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 {
		return 0, errors.New("invalid price")
	}
	return v, nil
}

func parsePct(s string) (int16, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 99 {
		return 0, errors.New("invalid pct")
	}
	return int16(v), nil
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
