package vk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Трекинг товаров из VK: та же логика, что в telegram.doTrack (скрейп → upsert
// товара → лимит тарифа → upsert подписки), но plain-text и без выбора типа
// уведомления — в VK подписка всегда «любое снижение», тонкая настройка в TG.

const (
	listMaxShown   = 20 // VK режет сообщения ~4096 символов
	listMaxButtons = 10 // лимит inline-клавиатуры VK
)

func (b *Bot) handleTrack(ctx context.Context, vkID int64, user *domain.User, rawURL string) {
	s, err := b.registry.FindByURL(rawURL)
	if err != nil {
		b.send(ctx, vkID, "Не могу распознать ссылку. Отправь ссылку на товар Wildberries.", nil)
		return
	}

	b.send(ctx, vkID, "⏳ Получаю данные о товаре...", nil)

	result, err := s.Scrape(ctx, rawURL)
	if err != nil {
		b.log.Error("vk: scrape on track", "url", rawURL, "err", err)
		b.send(ctx, vkID, "❌ Не удалось получить данные о товаре. Попробуй позже.", nil)
		return
	}

	product, err := b.prodRepo.Upsert(ctx, rawURL, result.Name, result.ImageURL, string(s.Marketplace()))
	if err != nil {
		b.log.Error("vk: upsert product", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	// Лимит тарифа: повторная ссылка на уже отслеживаемый товар лимит не расходует.
	plan := user.EffectivePlan(time.Now())
	active, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: count active subs", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	alreadyTracked := false
	for _, sub := range active {
		if sub.ProductID == product.ID {
			alreadyTracked = true
			break
		}
	}
	if !alreadyTracked && len(active) >= plan.MaxProduct {
		b.send(ctx, vkID, fmt.Sprintf(
			"🚫 Достигнут лимит тарифа %s: товаров %d из %d.\n\n"+
				"Отпишись от ненужного («Мои товары») или оформи тариф повыше — "+
				"тарифы пока в Telegram-боте @TryBerryBot, команда /plans.",
			plan.Title, len(active), plan.MaxProduct), menuKeyboard(user.TelegramID != 0))
		return
	}

	_, created, err := b.subRepo.Upsert(ctx, user.ID, product.ID, result.Price)
	if err != nil {
		b.log.Error("vk: upsert subscription", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	head := "✅ Добавил в отслеживание!"
	if !created {
		head = "🔄 Отслеживание возобновлено!"
	}
	b.send(ctx, vkID, fmt.Sprintf(
		"%s\n\n%s\n💰 Текущая цена: %.0f ₽\n\n"+
			"🔔 Напишу при любом снижении цены. Сменить тип уведомления (порог, процент) можно в Telegram-боте.",
		head, result.Name, result.Price), menuKeyboard(user.TelegramID != 0))
}

// handleList — список подписок + inline-кнопки отписки. prefix — строка над
// списком (например, подтверждение отписки).
func (b *Bot) handleList(ctx context.Context, vkID int64, user *domain.User, prefix string) {
	subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: get subscriptions", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if len(subs) == 0 {
		text := "📋 У тебя пока нет активных подписок.\n\nОтправь ссылку на товар Wildberries — начну отслеживать цену."
		if prefix != "" {
			text = prefix + "\n\n" + text
		}
		b.send(ctx, vkID, text, menuKeyboard(user.TelegramID != 0))
		return
	}

	var sb strings.Builder
	if prefix != "" {
		sb.WriteString(prefix + "\n\n")
	}
	fmt.Fprintf(&sb, "📋 Твои подписки — %d активных\n\n", len(subs))
	for i, sub := range subs {
		if i == listMaxShown {
			fmt.Fprintf(&sb, "… и ещё %d. Полный список — в Telegram-боте (/list).\n", len(subs)-listMaxShown)
			break
		}
		current := "нет данных"
		if sub.CurrentPrice > 0 {
			emoji := ""
			if sub.CurrentPrice < sub.FirstSeenPrice {
				emoji = "📉 "
			}
			current = fmt.Sprintf("%s%.0f ₽", emoji, sub.CurrentPrice)
		}
		fmt.Fprintf(&sb, "%d. %s\n   сейчас %s | при подписке %.0f ₽\n   %s\n\n",
			i+1, sub.ProductName, current, sub.FirstSeenPrice, sub.ProductURL)
	}
	sb.WriteString("Отписаться — кнопки «❌ номер» под сообщением 👇")

	// Inline-клавиатура отписки; постоянное меню при этом остаётся на месте.
	b.send(ctx, vkID, sb.String(), untrackKeyboard(subs))
}

// untrackKeyboard — inline-кнопки «❌ N» (VK: максимум 10 кнопок в inline).
func untrackKeyboard(subs []*domain.Subscription) *Keyboard {
	var rows [][]Button
	var row []Button
	for i, sub := range subs {
		if i == listMaxButtons {
			break
		}
		row = append(row, TextButton(
			fmt.Sprintf("❌ %d", i+1),
			fmt.Sprintf(`{"cmd":%q,"id":%d}`, cmdUntrack, sub.ID),
			ColorSecondary,
		))
		if len(row) == 5 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return &Keyboard{Inline: true, Buttons: rows}
}

func (b *Bot) handleUntrack(ctx context.Context, vkID int64, user *domain.User, subID int64) {
	if subID == 0 {
		b.send(ctx, vkID, b.welcomeText(user), menuKeyboard(user.TelegramID != 0))
		return
	}
	// id — из payload кнопки; гасим только подписку этого юзера.
	if err := b.subRepo.Deactivate(ctx, subID, user.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("vk: deactivate", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	b.handleList(ctx, vkID, user, "✅ Отслеживание отменено.")
}
