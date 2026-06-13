package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/yookassa"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
	"gitlab.com/KosovAndrey/tryberrybot/internal/vk"
)

const payDateLayout = "02.01.2006"

// setupPayments подключает оплату ЮKassa, если задан конфиг: платёжный сервис
// на витрины TG/VK (создание платежа) и консьюмер topic payments (применение).
// Без конфига — no-op (боты показывают заглушку), возвращает nil-консьюмер.
func setupPayments(
	ctx context.Context,
	log *slog.Logger,
	brokers []string,
	tgBot *telegram.Bot,
	vkBot *vk.Bot, // nil, если VK не включён
	paymentRepo *postgres.PaymentRepo,
	promoRepo *postgres.PromoRepo,
	referralRepo *postgres.ReferralRepo,
	userRepo *postgres.UserRepo,
	discounts *redisrepo.DiscountStore,
) *kafka.Consumer {
	shopID := getEnv("YOOKASSA_SHOP_ID", "")
	secret := getEnv("YOOKASSA_SECRET_KEY", "")
	if shopID == "" || secret == "" {
		log.Info("yookassa not configured — payments show stub")
		return nil
	}
	returnURL := getEnv("YOOKASSA_RETURN_URL", "https://t.me/TryBerryBot")
	// Ставка НДС в чеке. Самозанятый (НПД) не плательщик НДS → 1 = «без НДС».
	vatCode := 1
	if v, err := strconv.Atoi(getEnv("YOOKASSA_VAT_CODE", "1")); err == nil {
		vatCode = v
	}

	ykClient := yookassa.NewClient(shopID, secret)
	svc := payment.NewService(ykClient, paymentRepo, discounts, returnURL, vatCode, log)
	tgBot.SetPayments(svc)
	if vkBot != nil {
		vkBot.SetPayments(svc)
	}

	notifier := &paymentNotifier{tg: tgBot, vk: vkBot, log: log}
	applier := payment.NewApplier(paymentRepo, promoRepo, referralRepo, userRepo, discounts, notifier, log)

	consumer := kafka.NewConsumer(brokers, payment.TopicConfirmed, "payment-workers")
	go func() {
		log.Info("payments consumer started")
		err := consumer.Run(ctx, func(ctx context.Context, msg kafka.Message) error {
			ev, err := kafka.Decode[payment.ConfirmedEvent](msg)
			if err != nil {
				log.Error("decode payment event", "err", err)
				return nil // poison pill
			}
			return applier.Apply(ctx, ev)
		})
		if err != nil {
			log.Error("payments consumer stopped", "err", err)
		}
	}()
	log.Info("yookassa payments enabled", "return_url", returnURL)
	return consumer
}

// paymentNotifier реализует payment.Notifier: уведомляет во все привязанные
// платформы юзера (TG и/или VK) — оплату подтвердить важнее, чем экономить.
type paymentNotifier struct {
	tg  *telegram.Bot
	vk  *vk.Bot // nil, если VK не включён
	log *slog.Logger
}

func (n *paymentNotifier) PaymentSucceeded(ctx context.Context, buyer *domain.User, plan string, expiresAt time.Time) {
	p, _ := domain.PlanByName(plan)
	if buyer.TelegramID != 0 {
		n.tg.NotifyHTML(buyer.TelegramID, fmt.Sprintf(
			"✅ <b>Оплата прошла!</b>\n\n"+
				"Тариф <b>%s</b> активен до <b>%s</b>.\n"+
				"📦 До %d товаров · 🔎 до %d поиск-подписок.\n\n"+
				"Спасибо, что поддерживаешь бота 🍓",
			p.Title, expiresAt.Format(payDateLayout), p.MaxProduct, p.MaxSearch))
	}
	if buyer.VKID != nil && n.vk != nil {
		n.vk.Notify(ctx, *buyer.VKID, fmt.Sprintf(
			"✅ Оплата прошла!\n\n"+
				"Тариф %s активен до %s.\n"+
				"📦 До %d товаров · 🔎 до %d поиск-подписок.\n\n"+
				"Спасибо, что поддерживаешь бота 🍓",
			p.Title, expiresAt.Format(payDateLayout), p.MaxProduct, p.MaxSearch))
	}
}

func (n *paymentNotifier) ReferralPaid(ctx context.Context, referrer *domain.User, friendName string, days int, setPlan bool) {
	friend := "Твой друг"
	if friendName != "" {
		friend = "@" + friendName
	}
	tail := "Дни добавлены к твоему тарифу."
	if !setPlan {
		tail = "Награда учтена."
	}
	if referrer.TelegramID != 0 {
		n.tg.NotifyHTML(referrer.TelegramID, fmt.Sprintf(
			"🎉 %s оплатил тариф — тебе +%d %s тарифа за приглашение!\n%s",
			friend, days, domain.DaysWord(days), tail))
	}
	if referrer.VKID != nil && n.vk != nil {
		n.vk.Notify(ctx, *referrer.VKID, fmt.Sprintf(
			"🎉 %s оплатил тариф — тебе +%d %s тарифа за приглашение!\n%s",
			friend, days, domain.DaysWord(days), tail))
	}
}
