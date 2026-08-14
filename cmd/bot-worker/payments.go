package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/max"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/robokassa"
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
	maxBot *max.Bot, // nil, если MAX не включён
	paymentRepo *postgres.PaymentRepo,
	promoRepo *postgres.PromoRepo,
	referralRepo *postgres.ReferralRepo,
	userRepo *postgres.UserRepo,
	billingRepo *postgres.BillingSubscriptionRepo,
	discounts *redisrepo.DiscountStore,
) *kafka.Consumer {
	provider := setupProvider(log)
	if provider == nil {
		log.Info("payment provider not configured — payments show stub")
		return nil
	}

	svc := payment.NewService(provider, paymentRepo, promoRepo, discounts, billingRepo, log)
	tgBot.SetPayments(svc)
	tgBot.SetBilling(billingRepo)
	if vkBot != nil {
		vkBot.SetPayments(svc)
		vkBot.SetBilling(billingRepo)
	}
	if maxBot != nil {
		maxBot.SetPayments(svc)
		maxBot.SetBilling(billingRepo)
	}

	notifier := &paymentNotifier{tg: tgBot, vk: vkBot, mx: maxBot, log: log}
	applier := payment.NewApplier(paymentRepo, promoRepo, referralRepo, userRepo, billingRepo, discounts, notifier, log)

	// Шедулер автосписаний — только если провайдер умеет рекуррент (Робокасса).
	if provider.SupportsRecurring() {
		charger := payment.NewCharger(provider, billingRepo, paymentRepo, userRepo, notifier, log)
		interval := 1 * time.Hour
		if m, err := strconv.Atoi(getEnv("BILLING_CHARGE_INTERVAL_MINUTES", "60")); err == nil && m > 0 {
			interval = time.Duration(m) * time.Minute
		}
		go charger.Run(ctx, interval)
	}

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
	log.Info("payments enabled", "provider", provider.Name())
	return consumer
}

// setupProvider выбирает платёжный провайдер по флагу PAYMENT_PROVIDER.
// Возвращает nil, если выбранный провайдер не сконфигурирован (боты покажут
// заглушку). По умолчанию — ЮKassa (на период миграции на Робокассу).
func setupProvider(log *slog.Logger) payment.Provider {
	switch getEnv("PAYMENT_PROVIDER", domain.ProviderYooKassa) {
	case domain.ProviderRobokassa:
		login := getEnv("ROBOKASSA_MERCHANT_LOGIN", "")
		pw1 := getEnv("ROBOKASSA_PASSWORD1", "")
		pw2 := getEnv("ROBOKASSA_PASSWORD2", "")
		if login == "" || pw1 == "" || pw2 == "" {
			return nil
		}
		rk := robokassa.NewClient(robokassa.Config{
			Login:     login,
			Password1: pw1,
			Password2: pw2,
			IsTest:    getEnv("ROBOKASSA_IS_TEST", "0") == "1",
			SNO:       getEnv("ROBOKASSA_SNO", "npd"),
			HashType:  getEnv("ROBOKASSA_HASH_TYPE", ""),
		})
		fiscal := getEnv("ROBOKASSA_NPD", "1") == "1"
		// Рекуррент включаем только после активации менеджером Робокассы — иначе
		// ссылка подписки даёт ошибку 34. До этого доступна лишь разовая оплата.
		recurring := getEnv("ROBOKASSA_RECURRING", "0") == "1"
		return payment.NewRobokassaProvider(rk, fiscal, recurring)
	default:
		shopID := getEnv("YOOKASSA_SHOP_ID", "")
		secret := getEnv("YOOKASSA_SECRET_KEY", "")
		if shopID == "" || secret == "" {
			return nil
		}
		returnURL := getEnv("YOOKASSA_RETURN_URL", "https://t.me/TryBerryBot")
		// Ставка НДС в чеке. Самозанятый (НПД) не плательщик НДС → 1 = «без НДС».
		vatCode := 1
		if v, err := strconv.Atoi(getEnv("YOOKASSA_VAT_CODE", "1")); err == nil {
			vatCode = v
		}
		return payment.NewYooKassaProvider(yookassa.NewClient(shopID, secret), returnURL, vatCode)
	}
}

// paymentNotifier реализует payment.Notifier: уведомляет во все привязанные
// платформы юзера (TG и/или VK) — оплату подтвердить важнее, чем экономить.
type paymentNotifier struct {
	tg  *telegram.Bot
	vk  *vk.Bot  // nil, если VK не включён
	mx  *max.Bot // nil, если MAX не включён
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
	if buyer.MaxID != nil && n.mx != nil {
		n.mx.Notify(ctx, *buyer.MaxID, fmt.Sprintf(
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
	if referrer.MaxID != nil && n.mx != nil {
		n.mx.Notify(ctx, *referrer.MaxID, fmt.Sprintf(
			"🎉 %s оплатил тариф — тебе +%d %s тарифа за приглашение!\n%s",
			friend, days, domain.DaysWord(days), tail))
	}
}

func (n *paymentNotifier) SubscriptionChargeUpcoming(ctx context.Context, buyer *domain.User, plan string, amountKopecks int64, chargeAt time.Time) {
	p, _ := domain.PlanByName(plan)
	sum := domain.KopecksToRubString(amountKopecks)
	date := chargeAt.Format(payDateLayout)
	if buyer.TelegramID != 0 {
		n.tg.NotifyHTML(buyer.TelegramID, fmt.Sprintf(
			"🔁 <b>Скоро продлим подписку</b>\n\n"+
				"Тариф <b>%s</b>: %s ₽ спишутся автоматически <b>%s</b>.\n\n"+
				"Если продлевать не нужно — отмени автопродление в разделе «Мой тариф» (/myplan).",
			p.Title, sum, date))
	}
	if buyer.VKID != nil && n.vk != nil {
		n.vk.Notify(ctx, *buyer.VKID, fmt.Sprintf(
			"🔁 Скоро продлим подписку\n\nТариф %s: %s ₽ спишутся автоматически %s.\n\n"+
				"Если продлевать не нужно — отмени автопродление в разделе «Мой тариф».",
			p.Title, sum, date))
	}
	if buyer.MaxID != nil && n.mx != nil {
		n.mx.Notify(ctx, *buyer.MaxID, fmt.Sprintf(
			"🔁 Скоро продлим подписку\n\nТариф %s: %s ₽ спишутся автоматически %s.\n\n"+
				"Если продлевать не нужно — отмени автопродление в разделе «Мой тариф».",
			p.Title, sum, date))
	}
}

func (n *paymentNotifier) SubscriptionPaymentFailed(ctx context.Context, buyer *domain.User, plan string, willRetry bool) {
	p, _ := domain.PlanByName(plan)
	tail := "Повторим попытку списания позже — проверь, что на карте достаточно средств."
	if !willRetry {
		tail = "Автопродление остановлено. Чтобы продолжить пользоваться тарифом, оплати его заново в разделе «Тарифы»."
	}
	if buyer.TelegramID != 0 {
		n.tg.NotifyHTML(buyer.TelegramID, fmt.Sprintf(
			"⚠️ <b>Не удалось продлить подписку</b>\n\nТариф <b>%s</b>: автосписание не прошло.\n%s",
			p.Title, tail))
	}
	if buyer.VKID != nil && n.vk != nil {
		n.vk.Notify(ctx, *buyer.VKID, fmt.Sprintf(
			"⚠️ Не удалось продлить подписку\n\nТариф %s: автосписание не прошло.\n%s",
			p.Title, tail))
	}
	if buyer.MaxID != nil && n.mx != nil {
		n.mx.Notify(ctx, *buyer.MaxID, fmt.Sprintf(
			"⚠️ Не удалось продлить подписку\n\nТариф %s: автосписание не прошло.\n%s",
			p.Title, tail))
	}
}
