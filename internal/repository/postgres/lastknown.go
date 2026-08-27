package postgres

import (
	"context"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// LastKnown — что мы знаем о товаре из СВОЕЙ базы: карточка плюс последняя
// записанная цена. Собирается, когда площадка отказала: молчать «не удалось
// получить данные» нечестно, если история цен у нас есть и её можно показать.
type LastKnown struct {
	Product *domain.Product
	Price   float64
	At      time.Time
}

// FindLastKnown — карточка по каноническому URL и последняя цена из истории.
// ok=false — товара в базе нет ИЛИ цену по нему ни разу не писали: показывать
// нечего, вызывающий отвечает обычным «площадка временно не отвечает».
// priceRepo nilable (у каналов он опционален) — тогда тоже ok=false.
// Ошибку возвращаем отдельно от ok: она только для лога, ветку выбирает ok.
func FindLastKnown(ctx context.Context, prodRepo *ProductRepo, priceRepo *PriceHistoryRepo, canonicalURL string) (LastKnown, bool, error) {
	if prodRepo == nil || priceRepo == nil {
		return LastKnown{}, false, nil
	}
	p, found, err := prodRepo.FindByURL(ctx, canonicalURL)
	if err != nil || !found {
		return LastKnown{}, false, err
	}
	price, at, err := priceRepo.GetLatest(ctx, p.ID)
	if err != nil || price <= 0 {
		return LastKnown{Product: p}, false, err
	}
	return LastKnown{Product: p, Price: price, At: at}, true, nil
}
