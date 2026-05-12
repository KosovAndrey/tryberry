package domain

import "errors"

var (
	// ErrNotFound — сущность не найдена в БД
	ErrNotFound = errors.New("not found")

	// ErrAlreadySubscribed — пользователь уже подписан и подписка активна
	ErrAlreadySubscribed = errors.New("already subscribed")

	// ErrSubscriptionInactive — подписка существует но неактивна (для upsert)
	ErrSubscriptionInactive = errors.New("subscription inactive")
)
