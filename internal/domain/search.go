package domain

import "time"

// TriggerType — тип триггера уведомления для поиск-подписки.
//
// Различаются ТОЛЬКО логикой первого уведомления (см. §7H source-of-truth).
// После первого срабатывания для пары (подписка, товар) все три работают
// одинаково: «уведомить при любом снижении от цены последнего уведомления».
type TriggerType string

const (
	// TriggerBelowTarget — уведомить когда цена <= target_price.
	TriggerBelowTarget TriggerType = "below_target"
	// TriggerAnyDrop — уведомить при любом снижении от first_seen_price.
	TriggerAnyDrop TriggerType = "any_drop"
	// TriggerDiscountPct — уведомить когда скидка от first_seen_price >= discount_pct.
	TriggerDiscountPct TriggerType = "discount_pct"
)

// Valid проверяет что значение триггера допустимо.
func (t TriggerType) Valid() bool {
	switch t {
	case TriggerBelowTarget, TriggerAnyDrop, TriggerDiscountPct:
		return true
	default:
		return false
	}
}

// SearchQuery — отслеживаемая поисковая выдача маркетплейса.
// Шарится между всеми пользователями по normalized_url (дедупликация скрейпинга).
type SearchQuery struct {
	ID            int64
	Marketplace   string
	NormalizedURL string
	QueryText     string
	Filters       []byte // raw JSONB
	LastScrapedAt *time.Time
	CreatedAt     time.Time
}

// SearchSubscription — подписка пользователя на поисковую выдачу.
type SearchSubscription struct {
	ID            int64
	UserID        int64
	SearchQueryID int64
	TriggerType   TriggerType
	TargetPrice   *float64 // для below_target
	DiscountPct   *int16   // для discount_pct (1..99)
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// LastEvaluatedAt — когда воркер последний раз оценивал триггеры этой подписки
	// (throttle уведомлений по интервалу плана). nil → ещё ни разу.
	LastEvaluatedAt *time.Time

	// Поля для JOIN-запросов (не хранятся в самой таблице)
	TelegramID    int64
	QueryText     string
	NormalizedURL string

	// Тариф владельца — для вычисления интервала уведомлений в воркере
	// (источник истины domain.Plans). Заполняются JOIN users.
	OwnerPlan          string
	OwnerPlanExpiresAt *time.Time
}

// SearchResultItem — позиция товара в выдаче конкретного запроса.
type SearchResultItem struct {
	SearchQueryID int64
	ProductID     int64
	Position      int
	LastPrice     float64
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
}

// SearchSubProductBaseline — стартовая цена товара в момент подписки.
// Используется ТОЛЬКО для первого уведомления (см. §7H).
type SearchSubProductBaseline struct {
	SubscriptionID int64
	ProductID      int64
	FirstSeenPrice float64
	FirstSeenAt    time.Time
}

// SearchNotification — лог отправленного уведомления.
// Двойная роль: лог + baseline для последующих уведомлений (цена последней записи
// играет роль «текущего baseline» для пары подписка/товар).
type SearchNotification struct {
	ID             int64
	SubscriptionID int64
	ProductID      int64
	Price          float64
	SentAt         time.Time
}
