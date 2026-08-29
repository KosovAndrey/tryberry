package domain

import "errors"

var (
	// ErrNotFound — сущность не найдена в БД
	ErrNotFound = errors.New("not found")

	// ErrAlreadySubscribed — пользователь уже подписан и подписка активна
	ErrAlreadySubscribed = errors.New("already subscribed")

	// ErrSubscriptionInactive — подписка существует но неактивна (для upsert)
	ErrSubscriptionInactive = errors.New("subscription inactive")

	// ErrPlanActive — у юзера уже действует платный/выданный тариф. Активация
	// триала в этом случае запрещена: она ЗАМЕНЯЕТ plan_expires_at своими 10
	// днями и срезает оплаченный срок (инцидент 29.08.2026: Pro до 20.09 →
	// триал до 31.08 + win-back-пуш «триал заканчивается»).
	ErrPlanActive = errors.New("plan active")
)
