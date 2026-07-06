// cmd/notifier/winback.go
//
// Win-back конца триала: три касания с персональным discount-кодом −30%
// (дизайн: docs/TARIFF-FREE-SEARCH-LINK.md, «Промокоды» п.1; параметры —
// internal/domain/winback.go). Живёт внутри plan-reconciler-тика (синглтон,
// раз в PLAN_RECONCILE_INTERVAL_MINUTES): выборки дешёвые, гонок нет.
//
// Идемпотентность: строка trial_winbacks заводится один раз (PK user_id,
// ON CONFLICT DO NOTHING), каждая стадия отмечается stageN_sent_at ПОСЛЕ
// успешной отправки — сбой отправки ретраится следующим тиком, дублей нет.
// Ночью (вне окна 10:00–22:00 МСК) стадии просто ждут следующего дневного тика;
// дедлайн кода от этого не плывёт — он привязан к plan_expires_at.
package main

import (
	"context"
	"log/slog"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

// winbackTick — один проход всех стадий. Зовётся из reconciler-тика ДО
// paused-notice (юзеры с win-back-строкой получают стадию 2 вместо него).
func winbackTick(
	ctx context.Context,
	log *slog.Logger,
	wb *postgres.WinbackRepo,
	promos *postgres.PromoRepo,
	users *postgres.UserRepo,
	sender *deliverer,
	now time.Time,
) {
	if !domain.InSendWindow(now) {
		return
	}

	// Стадия 1, шаг А: завести код + строку новым кандидатам (триал истекает
	// в ближайшие WinbackStage1Lead). Отправка — шагом Б, единым путём с ретраями.
	cands, err := wb.ListStage1New(ctx, now.Add(domain.WinbackStage1Lead))
	if err != nil {
		log.Error("winback: list stage1 new", "err", err)
	}
	for _, c := range cands {
		code, err := domain.GenerateWinbackCode()
		if err != nil {
			log.Error("winback: generate code", "user_id", c.UserID, "err", err)
			continue
		}
		codeExpires := c.PlanExpiresAt.Add(domain.WinbackCodeTail)
		promoID, err := promos.Create(ctx, domain.PromoCode{
			Code:        code,
			Kind:        domain.PromoKindDiscount,
			DiscountPct: domain.WinbackDiscountPct,
			MaxUses:     1,
			ExpiresAt:   &codeExpires,
		})
		if err != nil {
			// В т.ч. крайне маловероятная коллизия кода — этот юзер попадёт в
			// ListStage1New следующим тиком с новым кодом.
			log.Error("winback: create promo", "user_id", c.UserID, "err", err)
			continue
		}
		if err := wb.Create(ctx, c.UserID, promoID, code, codeExpires); err != nil {
			log.Error("winback: create row", "user_id", c.UserID, "err", err)
		}
	}

	// Стадия 1, шаг Б: отправить всем с неотправленной первой стадией.
	stage1, err := wb.ListStage1Unsent(ctx)
	if err != nil {
		log.Error("winback: list stage1 unsent", "err", err)
	}
	for _, r := range stage1 {
		if err := sender.SendTrialWinback(ctx, r.UserID, 1, r.Code, r.CodeExpiresAt); err != nil {
			log.Error("winback: send stage1", "user_id", r.UserID, "err", err)
			continue
		}
		if err := wb.MarkStage(ctx, r.UserID, 1); err != nil {
			log.Error("winback: mark stage1", "user_id", r.UserID, "err", err)
		}
		// Глушим общий T-24h reminder («тариф скоро закончится») — цепочка
		// win-back уже сказала это, вторым сообщением было бы спамом.
		if err := users.MarkReminded(ctx, []int64{r.UserID}); err != nil {
			log.Warn("winback: mark reminded", "user_id", r.UserID, "err", err)
		}
	}
	if len(stage1) > 0 {
		log.Info("winback: stage1 sent", "count", len(stage1))
	}

	// Стадия 2: триал истёк, код жив, юзер не купил.
	stage2, err := wb.ListStage2Due(ctx)
	if err != nil {
		log.Error("winback: list stage2", "err", err)
	}
	for _, r := range stage2 {
		if err := sender.SendTrialWinback(ctx, r.UserID, 2, r.Code, r.CodeExpiresAt); err != nil {
			log.Error("winback: send stage2", "user_id", r.UserID, "err", err)
			continue
		}
		if err := wb.MarkStage(ctx, r.UserID, 2); err != nil {
			log.Error("winback: mark stage2", "user_id", r.UserID, "err", err)
		}
	}
	if len(stage2) > 0 {
		log.Info("winback: stage2 sent", "count", len(stage2))
	}

	// Стадия 3: последний звонок — до сгорания кода < WinbackStage3Lead,
	// код не погашен, юзер так и не купил.
	stage3, err := wb.ListStage3Due(ctx, domain.WinbackStage3Lead)
	if err != nil {
		log.Error("winback: list stage3", "err", err)
	}
	for _, r := range stage3 {
		if err := sender.SendTrialWinback(ctx, r.UserID, 3, r.Code, r.CodeExpiresAt); err != nil {
			log.Error("winback: send stage3", "user_id", r.UserID, "err", err)
			continue
		}
		if err := wb.MarkStage(ctx, r.UserID, 3); err != nil {
			log.Error("winback: mark stage3", "user_id", r.UserID, "err", err)
		}
	}
	if len(stage3) > 0 {
		log.Info("winback: stage3 sent", "count", len(stage3))
	}
}
