package telegram

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// Массовое добавление: пользователь шлёт несколько ссылок одним сообщением — заводим
// все товары с дефолтным триггером (любое снижение) и отвечаем ОДНОЙ сводкой, без
// интерактивных карточек на каждый (тип уведомления потом меняется в /list).
// doTrack (одиночный флоу с кнопками) намеренно не трогаем — у него богатый UI и нет
// тестов; ядро здесь отдельное и проще (репо-методы переиспользуем).

const bulkMaxURLs = 20 // потолок на сообщение: каждый item синхронно скрейпится

var bulkURLRe = regexp.MustCompile(`https?://[^\s]+`)

// trackableProductURLs вытаскивает из текста уникальные ТОВАРНЫЕ ссылки (распознаёт
// registry, не поисковые). Для роутинга «2+ ссылки → массовое добавление».
func (b *Bot) trackableProductURLs(text string) []string {
	matches := bulkURLRe.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		m = strings.TrimRight(m, ".,);]") // обрезаем хвостовую пунктуацию
		if m == "" || seen[m] || b.isSearchURL(m) {
			continue
		}
		if _, err := b.registry.FindByURL(m); err != nil {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

type bulkOutcome int

const (
	bulkFailed   bulkOutcome = iota // не распознали / скрейп упал / ошибка БД
	bulkAdded                       // новая подписка заведена (вкл. OOS → back_in_stock)
	bulkExisting                    // уже отслеживался / возобновлён
	bulkLimit                       // не поместился в лимит тарифа
)

type bulkItemResult struct {
	outcome   bulkOutcome
	name      string
	productID int64
}

// trackOne — добавление одного товара БЕЗ UI (ядро для bulk). Лимит проверяется по
// растущим в батче tracked/count. Дефолтный триггер — any_drop (Upsert), OOS-товар →
// back_in_stock (UpsertOutOfStock).
func (b *Bot) trackOne(ctx context.Context, user *domain.User, rawURL string, tracked map[int64]bool, count int, plan domain.Plan) bulkItemResult {
	s, err := b.registry.FindByURL(rawURL)
	if err != nil {
		b.log.Warn("bulk: url not recognized", "url", rawURL, "err", err)
		return bulkItemResult{outcome: bulkFailed}
	}
	result, _, err := b.registry.Scrape(ctx, rawURL)
	if err != nil {
		b.log.Warn("bulk: scrape failed", "url", rawURL, "marketplace", s.Marketplace(), "err", err)
		return bulkItemResult{outcome: bulkFailed}
	}
	product, err := b.prodRepo.Upsert(ctx, scraper.CanonicalProductURL(s.Marketplace(), rawURL), result.Name, result.ImageURL, string(s.Marketplace()),
		scraper.DisplayProductURL(s.Marketplace(), rawURL))
	if err != nil {
		b.log.Warn("bulk: upsert product failed", "url", rawURL, "err", err)
		return bulkItemResult{outcome: bulkFailed}
	}
	// Уже отслеживаемый товар лимит не расходует (это обновление, не новая подписка).
	if !tracked[product.ID] && count >= plan.MaxProduct {
		return bulkItemResult{outcome: bulkLimit, name: result.Name, productID: product.ID}
	}

	if !result.InStock {
		if err := b.prodRepo.SetInStock(ctx, product.ID, false); err != nil {
			b.log.Warn("bulk: set out of stock", "product_id", product.ID, "err", err)
		}
		_, created, err := b.subRepo.UpsertOutOfStock(ctx, user.ID, product.ID)
		if err != nil {
			b.log.Warn("bulk: upsert oos sub failed", "url", rawURL, "err", err)
			return bulkItemResult{outcome: bulkFailed}
		}
		return bulkItemResult{outcome: outcomeFor(created), name: result.Name, productID: product.ID}
	}

	_, created, err := b.subRepo.Upsert(ctx, user.ID, product.ID, result.Price)
	if err != nil {
		b.log.Warn("bulk: upsert sub failed", "url", rawURL, "err", err)
		return bulkItemResult{outcome: bulkFailed}
	}
	return bulkItemResult{outcome: outcomeFor(created), name: result.Name, productID: product.ID}
}

func outcomeFor(created bool) bulkOutcome {
	if created {
		return bulkAdded
	}
	return bulkExisting
}

// handleBulkTrack заводит все товарные ссылки из сообщения и отвечает одной сводкой.
func (b *Bot) handleBulkTrack(ctx context.Context, chatID int64, urls []string, user *domain.User) {
	truncated := false
	if len(urls) > bulkMaxURLs {
		urls = urls[:bulkMaxURLs]
		truncated = true
	}

	plan := user.EffectivePlan(time.Now())
	active, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("bulk: count active subs", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}
	tracked := make(map[int64]bool, len(active))
	for _, s := range active {
		tracked[s.ProductID] = true
	}
	count := len(active)

	b.reply(chatID, fmt.Sprintf("⏳ Добавляю %d товаров, секунду…", len(urls)))

	var added, existing, limited, failed int
	addedNames := make([]string, 0, len(urls))
	for _, u := range urls {
		res := b.trackOne(ctx, user, u, tracked, count, plan)
		switch res.outcome {
		case bulkAdded:
			added++
			if !tracked[res.productID] {
				tracked[res.productID] = true
				count++
			}
			if res.name != "" {
				addedNames = append(addedNames, res.name)
			}
		case bulkExisting:
			existing++
			tracked[res.productID] = true
		case bulkLimit:
			limited++
		default:
			failed++
		}
	}

	var sb strings.Builder
	sb.WriteString("📦 <b>Готово</b>\n")
	if added > 0 {
		fmt.Fprintf(&sb, "✅ Добавлено: <b>%d</b>\n", added)
	}
	if existing > 0 {
		fmt.Fprintf(&sb, "🔁 Уже отслеживались: %d\n", existing)
	}
	if limited > 0 {
		fmt.Fprintf(&sb, "🚫 Не поместились (лимит «%s» — %d товаров): %d. Апгрейд: /plans\n",
			plan.Title, plan.MaxProduct, limited)
	}
	if failed > 0 {
		fmt.Fprintf(&sb, "❌ Не удалось распознать/получить: %d\n", failed)
	}
	if truncated {
		fmt.Fprintf(&sb, "\n⚠️ За раз беру максимум %d ссылок — остальные пришли отдельным сообщением.\n", bulkMaxURLs)
	}
	if len(addedNames) > 0 {
		sb.WriteString("\n")
		const showN = 10
		for i, n := range addedNames {
			if i >= showN {
				fmt.Fprintf(&sb, "…и ещё %d\n", len(addedNames)-showN)
				break
			}
			fmt.Fprintf(&sb, "• %s\n", truncate(n, 50))
		}
		sb.WriteString("\nТип уведомления у новых — «любое снижение». Поменять можно в /list.")
	}
	b.reply(chatID, sb.String())
}
