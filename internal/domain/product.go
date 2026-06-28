package domain

import (
	"time"
)

type User struct {
	ID         int64
	TelegramID int64 // 0 = TG не привязан (VK-only юзер)
	Username   string
	CreatedAt  time.Time

	// Согласие на обработку ПД (152-ФЗ): когда подтверждено в боте.
	// nil = ещё не дано → бот показывает экран согласия и не пускает дальше.
	PDConsentAt *time.Time

	// VK-идентичность (vk_id = peer_id для отправки в ЛС VK)
	VKID *int64
	// MAX-идентичность (max_id = user_id для отправки в ЛС MAX)
	MaxID         *int64
	NotifyChannel string // auto | tg | vk | max | both | all

	// Тариф/лимиты
	Plan          string     // free | trial | basic | pro | unlimited
	PlanExpiresAt *time.Time // срок действия плана (nil = бессрочно)
	TrialUsed     bool       // триал уже активировался

	// Рефералка
	ReferredBy *int64 // users.id пригласившего (nil = пришёл сам)
}

type Product struct {
	ID          int64
	PublicID    string // стабильный токен для публичной страницы графика (/p/<public_id>)
	URL         string
	Name        string
	ImageURL    string
	Marketplace string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Subscription struct {
	ID        int64
	UserID    int64
	ProductID int64

	// BaselinePrice — «последняя опорная цена». При подписке = FirstSeenPrice,
	// после каждого уведомления обновляется на цену уведомления. Для повторных
	// срабатываний играет роль «цены последнего уведомления».
	BaselinePrice float64
	// FirstSeenPrice — НЕИЗМЕННАЯ цена на момент подписки. База ПЕРВОГО
	// срабатывания для any_drop и discount_pct (порог скидки не «уплывает»).
	FirstSeenPrice float64

	// Стратегия триггера (как у поиск-подписок).
	TriggerType TriggerType // below_target | any_drop | discount_pct
	TargetPrice *float64    // для below_target
	DiscountPct *int16      // для discount_pct (1..99)
	// Notified — было ли уже хоть одно уведомление по подписке (фаза первого vs
	// повторного срабатывания).
	Notified bool

	Active             bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	TelegramID         int64
	ProductMarketplace string

	// LastEvaluatedAt — когда notifier последний раз оценивал триггер этой
	// подписки (throttle уведомлений по интервалу плана). nil → ещё ни разу.
	LastEvaluatedAt *time.Time

	// Тариф владельца — для вычисления интервала проверки (источник истины
	// domain.Plans). Заполняются JOIN users.
	OwnerPlan          string
	OwnerPlanExpiresAt *time.Time

	// Поля для JOIN-запросов (не хранятся отдельно)
	ProductName     string
	ProductURL      string
	ProductImageURL string
	ProductPublicID string // для ссылки на страницу графика /p/<public_id>
	CurrentPrice    float64
}

type Notification struct {
	ID             int64
	SubscriptionID int64
	OldPrice       float64
	NewPrice       float64
	SentAt         time.Time
	IdempotencyKey string
}

// PriceEvent — сообщение в Kafka топике price-events.
// InStock/WasInStock — для триггера back_in_stock. Поля аддитивны: старые
// сообщения без них дают false, что лишь подавляет срабатывания (не шлёт
// ложные), поэтому порядок rolling-деплоя scraper/notifier некритичен.
type PriceEvent struct {
	ProductID   int64     `json:"product_id"`
	Marketplace string    `json:"marketplace"`
	OldPrice    float64   `json:"old_price"`
	NewPrice    float64   `json:"new_price"`
	RecordedAt  time.Time `json:"recorded_at"`
	InStock     bool      `json:"in_stock"`     // товар в наличии на этом скрейпе
	WasInStock  bool      `json:"was_in_stock"` // был ли в наличии на прошлом
}

// ScrapeTask — сообщение в Kafka топике scrape-tasks
type ScrapeTask struct {
	ProductID int64  `json:"product_id"`
	URL       string `json:"url"`
}
