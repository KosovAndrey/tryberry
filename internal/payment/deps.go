package payment

import (
	"context"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
)

// Узкие интерфейсы зависимостей Applier/Service — ровно те методы, что реально
// используются. Конкретные репозитории (*postgres.*Repo, *redisrepo.DiscountStore)
// их уже удовлетворяют, поэтому проводка в bot-worker не меняется. Нужны для
// юнит-тестов применения оплаты (идемпотентность/реф-награда/подписка) без БД.

// paymentMarker — идемпотентная отметка платежа применённым.
type paymentMarker interface {
	MarkSucceeded(ctx context.Context, paymentID int64, now time.Time) (postgres.AppliedPayment, bool, error)
}

// userGetter — загрузка пользователя по id (покупатель/реферер).
type userGetter interface {
	GetByID(ctx context.Context, id int64) (*domain.User, error)
}

// subscriptionApplier — заведение/продление рекуррентной подписки.
type subscriptionApplier interface {
	Activate(ctx context.Context, s domain.BillingSubscription) (int64, error)
	MarkRenewed(ctx context.Context, id, lastPaymentID int64, nextChargeAt time.Time) error
}

// promoRedeemer — гашение discount-кода после оплаты.
type promoRedeemer interface {
	RedeemDiscount(ctx context.Context, codeID, userID int64) error
}

// referralGranter — начисление реферальной награды (идемпотентно на стороне БД).
type referralGranter interface {
	GrantReward(ctx context.Context, referrerID, refereeID int64,
		event string, days int, capDays int,
		setPlan bool, plan string, expiresAt time.Time) (bool, error)
}

// discountClearer — снятие «ожидающей» скидки пользователя.
type discountClearer interface {
	Del(ctx context.Context, userID int64) error
}

// paymentCreator — заведение строки payments и привязка external id (для Service).
type paymentCreator interface {
	Create(ctx context.Context, p domain.Payment) (int64, error)
	SetYKID(ctx context.Context, id int64, ykID string) error
}

// discountReader — чтение «ожидающей» скидки пользователя (для Service).
type discountReader interface {
	Get(ctx context.Context, userID int64) (redisrepo.PendingDiscount, bool, error)
}

// promoCapacityChecker — есть ли у discount-кода свободные активации (гейт скидки
// на checkout, до оплаты). *postgres.PromoRepo удовлетворяет.
type promoCapacityChecker interface {
	Redeemable(ctx context.Context, codeID int64) (bool, error)
}
