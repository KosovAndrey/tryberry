package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// CallbackEvent — событие VK Callback API (сырой формат, публикуется в Kafka
// топик vk-updates как есть; secret проверяет и срезает api-ingestor).
type CallbackEvent struct {
	Type    string          `json:"type"`
	GroupID int64           `json:"group_id"`
	Object  json.RawMessage `json:"object"`
}

// messageNew — object события message_new. Payload приходит от нажатий
// text-кнопок клавиатуры (JSON-строка, см. TextButton).
type messageNew struct {
	Message struct {
		FromID  int64  `json:"from_id"`
		PeerID  int64  `json:"peer_id"`
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"message"`
}

// Команды кнопок (payload). Роутим по ним, а не по label — текст кнопки можно
// менять, не ломая обработку.
const (
	cmdProfile     = "profile"
	cmdLink        = "link"
	cmdUnlinkTG    = "unlinktg"  // отвязать Telegram (k=confirm — подтверждено)
	cmdLinkMax     = "linkmax"   // привязать MAX (выдать код vk2max)
	cmdUnlinkMax   = "unlinkmax" // отвязать MAX (k=confirm — подтверждено)
	cmdNotify      = "notify"    // цикл канала уведомлений по привязанным идентичностям
	cmdEmail       = "email"    // сменить email для чека 54-ФЗ
	cmdHelp        = "help"
	cmdAdd         = "add"
	cmdList        = "list"
	cmdUntrack     = "untrack"
	cmdPTrack      = "ptrack"      // тип триггера товарной подписки (k=any|stock|below|disc)
	cmdPTarget     = "ptgt"        // выбор подсказанной целевой цены (k=<rub>|manual)
	cmdListPage    = "lpage"       // навигация по страницам списка товаров (id=страница)
	cmdLSearchPage = "lspage"      // навигация по страницам списка поисков (id=страница)
	cmdSearch      = "search"      // как добавить поиск-подписку
	cmdLSearch     = "lsearch"     // список поиск-подписок
	cmdSTrack      = "strack"      // выбор типа триггера поиск-подписки (k=any|below|disc)
	cmdSFSkip      = "sfskip"      // «Без фильтра» на шаге текст-фильтра витрины продавца
	cmdSUntrack    = "suntrack"    // отписка от поиска
	cmdPlans       = "plans"       // витрина тарифов
	cmdPlanCard    = "plan"        // карточка тарифа (k=имя плана)
	cmdBuy         = "buy"         // разовая оплата (k=имя плана)
	cmdSub         = "sub"         // экран согласия на подписку (k=имя плана)
	cmdSubOk       = "subok"       // подтверждённое оформление подписки (k=имя плана)
	cmdSubCancel   = "subcancel"   // экран подтверждения отмены автопродления
	cmdSubCancelOk = "subcancelok" // отмена автопродления подтверждена
	cmdTrial       = "trial"
	cmdPromo       = "promo" // как активировать промокод
	cmdRef         = "ref"   // пригласить друга (код + статистика)
	cmdMerge       = "merge" // слияние аккаунтов (k = opt:<i>|confirm:<i>|back|cancel)
)

// payloadData — payload наших кнопок: {"cmd":"...","id":N,"k":"..."}.
type payloadData struct {
	Cmd  string `json:"cmd"`
	ID   int64  `json:"id,omitempty"`
	Kind string `json:"k,omitempty"`
}

func buttonPayload(cmd string) string {
	return fmt.Sprintf(`{"cmd":%q}`, cmd)
}

// parsePayload — payload кнопки (zero value — не кнопка/не наш формат).
func parsePayload(payload string) payloadData {
	var p payloadData
	if payload == "" {
		return p
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return payloadData{}
	}
	return p
}

// Bot — обработчик входящих событий VK. Фаза 2: привязка аккаунтов, трекинг
// товаров и поиск-подписки по ссылке, тарифы (заглушка оплаты), триал.
type Bot struct {
	client          *Client
	log             *slog.Logger
	userRepo        *postgres.UserRepo
	subRepo         *postgres.SubscriptionRepo
	prodRepo        *postgres.ProductRepo
	priceRepo       *postgres.PriceHistoryRepo // honest-price подсказки целевой цены (nilable)
	searchQueryRepo *postgres.SearchQueryRepo
	searchSubRepo   *postgres.SearchSubscriptionRepo
	promoRepo       *postgres.PromoRepo
	referralRepo    *postgres.ReferralRepo
	registry        *scraper.Registry
	resolver        *scraper.LinkResolver // короткие ссылки приложений (ozon.ru/t/…, market.yandex.ru/cc/…) — паритет с TG
	linkCodes       *redisrepo.LinkCodeStore
	rdb             *redis.Client // FSM ввода порога (может быть nil)
	botURL          string        // ссылка на VK-бота для приглашений ("" — не показывать)
	maxBotURL       string        // ссылка на MAX-бота для кнопки привязки ("" — не показывать)
	chartBaseURL    string        // PUBLIC_BASE_URL для ссылки «📈 График цены» → /p/<public_id>; "" — не показывать
	adminIDs        map[int64]bool // VK_ADMIN_IDS — операторы для админ-команд (grant/revoke/promo…)

	// Оплата (как в TG): payments == nil → заглушка; discounts хранит
	// «ожидающую скидку» (nil без redis); billing — рекуррентные подписки.
	payments  *payment.Service
	discounts *redisrepo.DiscountStore
	billing   *postgres.BillingSubscriptionRepo
}

// SetPayments подключает платёжный сервис (опционально).
func (b *Bot) SetPayments(svc *payment.Service) {
	b.payments = svc
}

// SetBilling подключает репозиторий подписок (статус/отмена).
func (b *Bot) SetBilling(repo *postgres.BillingSubscriptionRepo) {
	b.billing = repo
}

func NewBot(
	client *Client,
	log *slog.Logger,
	userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo,
	prodRepo *postgres.ProductRepo,
	priceRepo *postgres.PriceHistoryRepo,
	searchQueryRepo *postgres.SearchQueryRepo,
	searchSubRepo *postgres.SearchSubscriptionRepo,
	promoRepo *postgres.PromoRepo,
	referralRepo *postgres.ReferralRepo,
	registry *scraper.Registry,
	linkCodes *redisrepo.LinkCodeStore,
	rdb *redis.Client,
	botURL string,
	maxBotURL string,
	chartBaseURL string,
	adminIDs map[int64]bool,
) *Bot {
	var discounts *redisrepo.DiscountStore
	if rdb != nil {
		discounts = redisrepo.NewDiscountStore(rdb)
	}
	return &Bot{
		client:          client,
		log:             log,
		userRepo:        userRepo,
		subRepo:         subRepo,
		prodRepo:        prodRepo,
		priceRepo:       priceRepo,
		searchQueryRepo: searchQueryRepo,
		searchSubRepo:   searchSubRepo,
		promoRepo:       promoRepo,
		referralRepo:    referralRepo,
		registry:        registry,
		resolver:        scraper.NewLinkResolver(0),
		linkCodes:       linkCodes,
		rdb:             rdb,
		botURL:          botURL,
		maxBotURL:       maxBotURL,
		chartBaseURL:    chartBaseURL,
		adminIDs:        adminIDs,
		discounts:       discounts,
	}
}

// isAdmin — VK-оператор (по vk_id из VK_ADMIN_IDS). Зеркало telegram.Bot.isAdmin.
func (b *Bot) isAdmin(vkID int64) bool {
	return b.adminIDs[vkID]
}

// chartURL — публичная ссылка на график товара (chartBaseURL + "/p/" + publicID),
// либо "" (сайт не задан / нет токена). Зеркалит telegram.Bot.chartURL.
func (b *Bot) chartURL(publicID string) string {
	if b.chartBaseURL == "" || publicID == "" {
		return ""
	}
	return b.chartBaseURL + "/p/" + publicID
}

// chartURLForSub — ссылка на график по id подписки (когда public_id товара под
// рукой нет: нажатие кнопки типа триггера). Зеркалит telegram.Bot.chartURLForSub.
func (b *Bot) chartURLForSub(ctx context.Context, subID int64) string {
	if b.chartBaseURL == "" {
		return ""
	}
	sub, err := b.subRepo.GetByID(ctx, subID)
	if err != nil {
		return ""
	}
	p, err := b.prodRepo.GetByID(ctx, sub.ProductID)
	if err != nil {
		return ""
	}
	return b.chartURL(p.PublicID)
}

// HandleEvent — точка входа для события из Kafka.
func (b *Bot) HandleEvent(ctx context.Context, ev CallbackEvent) {
	if ev.Type != "message_new" {
		return // лайки/подписки и прочее — не интересуют в фазе 1
	}
	var m messageNew
	if err := json.Unmarshal(ev.Object, &m); err != nil {
		b.log.Error("vk: decode message_new", "err", err)
		return
	}
	// Только личные сообщения (peer_id > 2e9 — чаты, их игнорируем).
	if m.Message.PeerID != m.Message.FromID {
		return
	}
	b.handleMessage(ctx, m.Message.FromID, strings.TrimSpace(m.Message.Text), m.Message.Payload)
}

// menuKeyboard — постоянная клавиатура: до привязки на первом месте
// «Привязать Telegram».
func menuKeyboard(linked bool) *Keyboard {
	var rows [][]Button
	if !linked {
		rows = append(rows, []Button{
			TextButton("🔗 Привязать Telegram", buttonPayload(cmdLink), ColorPrimary),
		})
	}
	rows = append(rows,
		[]Button{
			TextButton("➕ Добавить товар", buttonPayload(cmdAdd), ColorPrimary),
			TextButton("📋 Мои товары", buttonPayload(cmdList), ColorPrimary),
		},
		[]Button{
			TextButton("🔎 Поиск по ссылке", buttonPayload(cmdSearch), ColorSecondary),
			TextButton("📡 Мои поиски", buttonPayload(cmdLSearch), ColorSecondary),
		},
		[]Button{
			TextButton("💳 Тарифы", buttonPayload(cmdPlans), ColorSecondary),
			TextButton("🎁 Триал", buttonPayload(cmdTrial), ColorSecondary),
		},
		[]Button{
			TextButton("🎟 Промокод", buttonPayload(cmdPromo), ColorSecondary),
			TextButton("👥 Пригласить друга", buttonPayload(cmdRef), ColorSecondary),
		},
		[]Button{
			TextButton("👤 Профиль", buttonPayload(cmdProfile), ColorSecondary),
			TextButton("❓ Помощь", buttonPayload(cmdHelp), ColorSecondary),
		},
	)
	return &Keyboard{Buttons: rows}
}

func (b *Bot) handleMessage(ctx context.Context, vkID int64, text, payload string) {
	user, err := b.userRepo.UpsertVK(ctx, vkID)
	if err != nil {
		b.log.Error("vk: upsert user", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	kb := menuKeyboard(user.TelegramID != 0)

	// Короткие ссылки приложений (ozon.ru/t/…, market.yandex.ru/cc/…) → канонический
	// URL до любого разбора текста; без "://" в тексте — no-op без сети. Паритет с TG.
	text = b.resolver.ExpandInText(ctx, text)

	// Слэш-команды (admin + /myplan + алиасы команд TG) — паритет с TG. Прерывают
	// любой незавершённый ввод; кнопки (payload) сюда не попадают.
	if strings.HasPrefix(text, "/") && b.handleSlashCommand(ctx, vkID, user, text) {
		return
	}

	p := parsePayload(payload)
	lower := strings.ToLower(text)
	if p.Cmd == "" {
		switch lower {
		case "профиль", "profile":
			p.Cmd = cmdProfile
		case "помощь", "help", "начать", "start":
			p.Cmd = cmdHelp
		case "мои товары", "список", "list":
			p.Cmd = cmdList
		case "добавить товар", "добавить":
			p.Cmd = cmdAdd
		case "мои поиски":
			p.Cmd = cmdLSearch
		case "тарифы":
			p.Cmd = cmdPlans
		case "триал":
			p.Cmd = cmdTrial
		case "промокод":
			p.Cmd = cmdPromo
		case "email", "почта", "емейл":
			p.Cmd = cmdEmail
		}
	}

	if p.Cmd != "" {
		// Любая кнопка/команда прерывает незавершённый ввод порога, email и промокода.
		b.clearSearchFSM(ctx, vkID)
		b.clearTrackFSM(ctx, vkID)
		b.clearEmailFSM(ctx, vkID)
		b.clearPromoFSM(ctx, vkID)
	} else {
		// Ждём email для чека 54-ФЗ перед оплатой? (сильный модальный режим).
		if fsm, ok := b.getEmailFSM(ctx, vkID); ok {
			metrics.VKMessages.WithLabelValues("email_input").Inc()
			b.handleEmailInput(ctx, vkID, user, text, fsm)
			return
		}
		// Ждём промокод (кнопка «Промокод» в меню/карточке)? Модальный режим.
		if fsm, ok := b.getPromoFSM(ctx, vkID); ok {
			metrics.VKMessages.WithLabelValues("promo_input").Inc()
			b.handlePromoInput(ctx, vkID, user, text, fsm)
			return
		}
		// «привязать <КОД>» / «link <КОД>» — предъявление кода, выданного в TG.
		// Код регистронезависим (Redeem приводит к UPPER), так что lower не мешает.
		// Кнопка «Привязать Telegram» сюда не попадает — у неё payload cmd=link.
		if rest, ok := cutAnyPrefix(lower, "привязать ", "link "); ok {
			metrics.VKMessages.WithLabelValues("link_code").Inc()
			b.handleLink(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// «промокод <КОД>» — активация промокода.
		if rest, ok := cutAnyPrefix(lower, "промокод ", "promo "); ok {
			metrics.VKMessages.WithLabelValues("promo_code").Inc()
			b.clearSearchFSM(ctx, vkID)
			b.clearTrackFSM(ctx, vkID)
			b.handlePromoCode(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// «друг <КОД>» — код приглашения (= users.id реферера).
		if rest, ok := cutAnyPrefix(lower, "друг ", "friend "); ok {
			metrics.VKMessages.WithLabelValues("ref_code").Inc()
			b.clearSearchFSM(ctx, vkID)
			b.clearTrackFSM(ctx, vkID)
			b.handleRefCode(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// Ждём число (порог/процент)? Сначала товарный FSM, потом поисковый.
		if fsm, ok := b.getTrackFSM(ctx, vkID); ok {
			metrics.VKMessages.WithLabelValues("track_threshold").Inc()
			b.handleTrackThreshold(ctx, vkID, user, text, fsm)
			return
		}
		if fsm, ok := b.getSearchFSM(ctx, vkID); ok {
			if fsm.SellerURL != "" {
				metrics.VKMessages.WithLabelValues("seller_text_filter").Inc()
				b.handleSellerTextFilter(ctx, vkID, user, text, fsm)
			} else {
				metrics.VKMessages.WithLabelValues("search_threshold").Inc()
				b.handleSearchThreshold(ctx, vkID, user, text, fsm)
			}
			return
		}
		// 2+ товарных ссылок одним сообщением → массовое добавление одной сводкой
		// (раньше одиночных веток, как в TG).
		if prods := b.trackableProductURLs(text); len(prods) >= 2 {
			metrics.VKMessages.WithLabelValues("bulk_track").Inc()
			b.handleBulkTrack(ctx, vkID, prods, user)
			return
		}
		// Поисковая ссылка → флоу поиск-подписки.
		if _, err := b.registry.FindSearchByURL(text); err == nil {
			metrics.VKMessages.WithLabelValues("search_url").Inc()
			b.startSearchTrack(ctx, vkID, user, text)
			return
		}
		// Ссылка на товар → трекинг.
		if _, err := b.registry.FindByURL(text); err == nil {
			metrics.VKMessages.WithLabelValues("track_url").Inc()
			b.handleTrack(ctx, vkID, user, text)
			return
		}
		// Витрина продавца с буквенной ссылкой (/seller/имя) → резолвим слаг в
		// числовой id (через wbaas-токен) и заводим как обычную seller-подписку.
		if slug, ok := domain.SellerVanitySlug(text); ok {
			metrics.VKMessages.WithLabelValues("seller_vanity").Inc()
			if id, err := b.registry.ResolveSellerVanity(ctx, slug); err == nil && id != "" {
				b.startSearchTrack(ctx, vkID, user, domain.RewriteSellerVanity(text, id))
			} else {
				if err != nil {
					b.log.Warn("vk: resolve seller vanity", "slug", slug, "err", err)
				}
				b.send(ctx, vkID, "🏬 Не получилось открыть этот магазин по буквенной ссылке. Попробуй ссылку с числовым номером (вида /seller/250021611) — её даёт кнопка «Поделиться» на странице продавца в приложении WB.", kb)
			}
			return
		}
	}

	if p.Cmd != "" {
		metrics.VKMessages.WithLabelValues(p.Cmd).Inc()
	} else {
		metrics.VKMessages.WithLabelValues("other").Inc()
	}

	switch p.Cmd {
	case cmdProfile:
		b.sendProfile(ctx, vkID, user)
	case cmdNotify:
		b.toggleNotify(ctx, vkID, user)
	case cmdLink:
		b.issueLinkCode(ctx, vkID, user)
	case cmdUnlinkTG:
		b.handleUnlinkTG(ctx, vkID, user, p.Kind == "confirm")
	case cmdLinkMax:
		b.issueLinkCodeMax(ctx, vkID, user)
	case cmdUnlinkMax:
		b.handleUnlinkMax(ctx, vkID, user, p.Kind == "confirm")
	case cmdHelp:
		b.send(ctx, vkID, b.helpText(user), kb)
	case cmdAdd:
		b.send(ctx, vkID, "➕ Отправь мне ссылку на товар Wildberries — начну отслеживать цену.\n\n"+
			"Пример:\nhttps://www.wildberries.ru/catalog/252334498/detail.aspx", kb)
	case cmdList:
		b.handleList(ctx, vkID, user, "")
	case cmdListPage:
		b.showProductList(ctx, vkID, user, "", int(p.ID))
	case cmdUntrack:
		b.handleUntrack(ctx, vkID, user, p.ID)
	case cmdPTrack:
		b.handleProductTrigger(ctx, vkID, user, p)
	case cmdPTarget:
		b.handleProductTarget(ctx, vkID, user, p)
	case cmdSearch:
		b.send(ctx, vkID, "🔎 Поиск по ссылке\n\n"+
			"Отправь ссылку на поисковую выдачу Wildberries — буду следить за всей выдачей и напишу, когда товары подешевеют.\n\n"+
			"Как получить ссылку: на сайте WB введи запрос в поиск и скопируй адрес страницы.\n\n"+
			"Пример:\nhttps://www.wildberries.ru/catalog/0/search.aspx?search=наушники", kb)
	case cmdLSearch:
		b.handleListSearch(ctx, vkID, user, "")
	case cmdLSearchPage:
		b.showSearchList(ctx, vkID, user, "", int(p.ID))
	case cmdSTrack:
		b.handleSearchTrigger(ctx, vkID, user, p)
	case cmdSFSkip:
		b.handleSellerSkipFilter(ctx, vkID, user)
	case cmdSUntrack:
		b.handleUntrackSearch(ctx, vkID, user, p.ID)
	case cmdPlans:
		b.sendPlans(ctx, vkID, user)
	case cmdPlanCard:
		b.sendPlanCard(ctx, vkID, user, p.Kind)
	case cmdBuy:
		b.handlePlanBuy(ctx, vkID, user, p.Kind)
	case cmdSub:
		b.sendSubConsent(ctx, vkID, user, p.Kind)
	case cmdSubOk:
		b.handleSubBuy(ctx, vkID, user, p.Kind)
	case cmdSubCancel:
		b.handleSubCancelConfirm(ctx, vkID, user)
	case cmdSubCancelOk:
		b.handleSubCancel(ctx, vkID, user)
	case cmdTrial:
		b.handleTrial(ctx, vkID, user)
	case cmdPromo:
		// Меню (k="") → ввод кода с возвратом в тарифы; карточка (k=план) → возврат
		// на карточку. Запускаем диалог ввода кода (FSM).
		b.promptPromo(ctx, vkID, user, p.Kind)
	case cmdEmail:
		b.promptChangeEmail(ctx, vkID, user)
	case cmdRef:
		b.sendRef(ctx, vkID, user)
	case cmdMerge:
		b.handleMergeAction(ctx, vkID, user, p.Kind)
	default:
		b.send(ctx, vkID, b.welcomeText(user), kb)
	}
}

// handleLink — гасим код привязки (направление tg2vk: код выдан в Telegram,
// предъявлен здесь — владение обоими аккаунтами доказано).
func (b *Bot) handleLink(ctx context.Context, vkID int64, vkUser *domain.User, code string) {
	if b.linkCodes == nil {
		b.send(ctx, vkID, "Привязка временно недоступна, попробуй позже.", nil)
		return
	}
	dir, tgUserID, err := b.linkCodes.Redeem(ctx, code)
	// Код, выданный в TG (tg2vk) или MAX (max2vk) для предъявления здесь. В обоих
	// случаях привязываем нашу VK-идентичность к аккаунту-эмитенту (tgUserID).
	if err != nil || (dir != domain.LinkDirTG2VK && dir != domain.LinkDirMax2VK) {
		// Неверный/истёкший код и чужое направление неразличимы для юзера.
		b.send(ctx, vkID, "Код не подошёл 😕 Проверь, что скопировал его целиком, "+
			"или получи новый в Telegram/MAX-боте: «Привязать VK» (код живёт 15 минут).",
			menuKeyboard(false))
		return
	}

	if err := b.userRepo.LinkVK(ctx, tgUserID, vkID); err != nil {
		if errors.Is(err, domain.ErrVKAccountBusy) {
			// Оба аккаунта непустые — предлагаем объединение (код уже доказал
			// владение обеими сторонами).
			b.startMergeFlow(ctx, vkID, vkUser, tgUserID)
			return
		}
		b.log.Error("vk: link", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	b.send(ctx, vkID, "Готово! 🎉 Аккаунты связаны.\n\n"+
		"Теперь в Профиле (кнопка внизу) можно выбрать, куда слать уведомления — "+
		"в Telegram, сюда или в оба места.",
		menuKeyboard(true))
}

// sendProfile — экран профиля: идентичности, тариф, использование лимитов,
// настройка уведомлений и управление привязкой Telegram (только TG — VK здесь
// менять нельзя, симметрично TG-боту, где меняется только VK).
func (b *Bot) sendProfile(ctx context.Context, vkID int64, u *domain.User) {
	now := time.Now()
	plan := u.EffectivePlan(now)

	prod, _ := b.subRepo.CountActiveByUserID(ctx, u.ID)
	srch, _ := b.searchSubRepo.CountActiveByUserID(ctx, u.ID)

	tg := "❌ не привязан"
	if u.TelegramID != 0 {
		tg = "✅ привязан"
	}
	mx := "❌ не привязан"
	if u.MaxID != nil {
		mx = "✅ привязан"
	}
	planLine := "Тариф: " + plan.Title
	if u.PlanExpiresAt != nil && !u.PlanExpired(now) {
		planLine += " (до " + u.PlanExpiresAt.Format(dateLayout) + ")"
	}

	var sb strings.Builder
	sb.WriteString("👤 Профиль\n\n")
	fmt.Fprintf(&sb, "VK: ✅ привязан\nTelegram: %s\nMAX: %s\n%s\n", tg, mx, planLine)
	fmt.Fprintf(&sb, "📦 Товаров: %d из %d · 🔎 Поисков: %d из %d\n", prod, plan.MaxProduct, srch, plan.MaxSearch)

	// Email для чека 54-ФЗ — только когда оплата подключена.
	emailLabel := "✉️ Указать email"
	if b.payments != nil {
		if email, err := b.userRepo.GetEmail(ctx, u.ID); err == nil && email != "" {
			emailLabel = "✉️ Изменить email"
			fmt.Fprintf(&sb, "✉️ Email для чека: %s\n", email)
		} else {
			sb.WriteString("✉️ Email для чека: ❌ не указан\n")
		}
	}

	subLine, hasSub := b.subscriptionLine(ctx, u.ID)
	sb.WriteString(subLine)

	// Унифицированный inline-профиль (зеркало MAX-бота): управление привязками
	// TG и MAX, уведомления, email, подписка. Навигация — кнопкой «◀️ Меню».
	var rows [][]Button
	// Цикл канала уведомлений доступен при ≥2 идентичностях.
	if vkIdentityCount(u) >= 2 {
		fmt.Fprintf(&sb, "🔔 Уведомления: %s\n", domain.NotifyChannelTitle(u.NotifyChannel))
		rows = append(rows, []Button{TextButton("🔔 Уведомления: "+domain.NotifyChannelTitle(u.NotifyChannel),
			buttonPayload(cmdNotify), ColorPrimary)})
	}
	if u.TelegramID != 0 {
		rows = append(rows, []Button{TextButton("🔗 Отвязать Telegram", buttonPayload(cmdUnlinkTG), ColorSecondary)})
	} else {
		rows = append(rows, []Button{TextButton("🔗 Привязать Telegram", buttonPayload(cmdLink), ColorSecondary)})
	}
	if u.MaxID != nil {
		rows = append(rows, []Button{TextButton("🔗 Отвязать MAX", buttonPayload(cmdUnlinkMax), ColorSecondary)})
	} else {
		rows = append(rows, []Button{TextButton("🔗 Привязать MAX", buttonPayload(cmdLinkMax), ColorSecondary)})
	}
	if b.payments != nil {
		rows = append(rows, []Button{TextButton(emailLabel, buttonPayload(cmdEmail), ColorSecondary)})
	}
	if hasSub {
		rows = append(rows, []Button{TextButton("🚫 Отменить автопродление", buttonPayload(cmdSubCancel), ColorSecondary)})
	}
	rows = append(rows, []Button{TextButton("◀️ Меню", buttonPayload(""), ColorSecondary)})
	b.send(ctx, vkID, sb.String(), &Keyboard{Inline: true, Buttons: rows})
}

// vkNotifyCycle — порядок переключения канала уведомлений: vk → tg → max → all,
// где присутствуют только привязанные идентичности (VK всегда базовый в этом боте).
func vkNotifyCycle(u *domain.User) []string {
	cycle := []string{domain.NotifyVK}
	if u.TelegramID != 0 {
		cycle = append(cycle, domain.NotifyTG)
	}
	if u.MaxID != nil {
		cycle = append(cycle, domain.NotifyMax)
	}
	return append(cycle, domain.NotifyAll)
}

// vkIdentityCount — сколько идентичностей привязано (VK всегда есть в этом боте).
func vkIdentityCount(u *domain.User) int {
	n := 1
	if u.TelegramID != 0 {
		n++
	}
	if u.MaxID != nil {
		n++
	}
	return n
}

// toggleNotify — циклически переключить канал уведомлений по доступным
// идентичностям (при ≥2 привязках): vk → tg → max → all → vk, где присутствуют
// только привязанные каналы. Легаси-both трактуем как начало цикла.
func (b *Bot) toggleNotify(ctx context.Context, vkID int64, u *domain.User) {
	if vkIdentityCount(u) < 2 {
		b.sendProfile(ctx, vkID, u)
		return
	}
	cycle := vkNotifyCycle(u)

	cur := 0
	for i, c := range cycle {
		if c == u.NotifyChannel {
			cur = i
			break
		}
	}
	next := cycle[(cur+1)%len(cycle)]
	if err := b.userRepo.SetNotifyChannel(ctx, u.ID, next); err != nil {
		b.log.Error("vk: set notify channel", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	u.NotifyChannel = next
	b.sendProfile(ctx, vkID, u)
}

// issueLinkCode — выдать код привязки Telegram (направление vk2tg: код выдан
// здесь, предъявляется в TG-боте).
func (b *Bot) issueLinkCode(ctx context.Context, vkID int64, u *domain.User) {
	if u.TelegramID != 0 {
		b.send(ctx, vkID, "Твой аккаунт уже связан с Telegram ✅\n\n"+
			"Сменить привязку: сначала «Отвязать Telegram» в Профиле, потом привязать заново.",
			menuKeyboard(true))
		return
	}
	if b.linkCodes == nil {
		b.send(ctx, vkID, "Привязка временно недоступна, попробуй позже.", nil)
		return
	}
	code, err := b.linkCodes.Issue(ctx, u.ID, domain.LinkDirVK2TG)
	if err != nil {
		if errors.Is(err, domain.ErrLinkCodeRateLimited) {
			b.send(ctx, vkID, "⏳ Код уже выдан — подожди минуту и попробуй снова, если не успел его использовать.", nil)
			return
		}
		b.log.Error("vk: issue link code", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	ttlMin := int(domain.LinkCodeTTL.Minutes())
	b.send(ctx, vkID, fmt.Sprintf(
		"🔗 Привязка Telegram\n\n"+
			"1. Открой Telegram-бота @TryBerryBot (t.me/TryBerryBot)\n"+
			"2. Отправь ему сообщение:\n\nпривязать %s\n\n"+
			"Код действует %d минут и работает один раз. Никому его не пересылай — "+
			"это ключ к твоему аккаунту.\n\n"+
			"Если уже пользуешься Telegram-ботом, можно наоборот: в TG Профиль → "+
			"«Привязать VK» и прислать код сюда.",
		code, ttlMin), menuKeyboard(false))
}

// issueLinkCodeMax — выдать код привязки MAX (направление vk2max: код выдан здесь,
// предъявляется в MAX-боте, который гасит его через LinkMax).
func (b *Bot) issueLinkCodeMax(ctx context.Context, vkID int64, u *domain.User) {
	if u.MaxID != nil {
		b.send(ctx, vkID, "Твой аккаунт уже связан с MAX ✅\n\n"+
			"Сменить привязку: сначала «Отвязать MAX» в Профиле, потом привязать заново.",
			nil)
		return
	}
	if b.linkCodes == nil {
		b.send(ctx, vkID, "Привязка временно недоступна, попробуй позже.", nil)
		return
	}
	code, err := b.linkCodes.Issue(ctx, u.ID, domain.LinkDirVK2Max)
	if err != nil {
		if errors.Is(err, domain.ErrLinkCodeRateLimited) {
			b.send(ctx, vkID, "⏳ Код уже выдан — подожди минуту и попробуй снова, если не успел его использовать.", nil)
			return
		}
		b.log.Error("vk: issue link code max", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	ttlMin := int(domain.LinkCodeTTL.Minutes())
	text := fmt.Sprintf(
		"🔗 Привязка MAX\n\n"+
			"1. Открой нашего бота в MAX\n"+
			"2. Отправь ему сообщение:\n\nпривязать %s\n\n"+
			"Код действует %d минут и работает один раз. Никому его не пересылай — "+
			"это ключ к твоему аккаунту.",
		code, ttlMin)
	var kb *Keyboard
	if b.maxBotURL != "" {
		kb = &Keyboard{Inline: true, Buttons: [][]Button{
			{LinkButton("Открыть бота в MAX", b.maxBotURL)},
		}}
	}
	b.send(ctx, vkID, text, kb)
}

// handleUnlinkTG — отвязка Telegram из VK (с подтверждением). VK-идентичность
// отсюда отвязать нельзя — только Telegram (зеркально TG-боту).
func (b *Bot) handleUnlinkTG(ctx context.Context, vkID int64, u *domain.User, confirmed bool) {
	if u.TelegramID == 0 {
		b.sendProfile(ctx, vkID, u)
		return
	}
	if !confirmed {
		kb := &Keyboard{Inline: true, Buttons: [][]Button{
			{TextButton("⚠️ Да, отвязать", fmt.Sprintf(`{"cmd":%q,"k":"confirm"}`, cmdUnlinkTG), ColorSecondary)},
			{TextButton("◀️ Отмена", buttonPayload(cmdProfile), ColorPrimary)},
		}}
		b.send(ctx, vkID, "Отвязать Telegram от этого аккаунта?\n\n"+
			"Подписки и тариф останутся здесь, в VK. Telegram-аккаунт начнёт с чистого листа "+
			"при следующем заходе в TG-бота.", kb)
		return
	}
	if err := b.userRepo.UnlinkTG(ctx, u.ID); err != nil {
		b.log.Error("vk: unlink tg", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	u.TelegramID = 0
	b.send(ctx, vkID, "✅ Telegram отвязан. Уведомления теперь приходят сюда, в VK.\n\n"+
		"Привязать снова — кнопка «Привязать Telegram» внизу.", menuKeyboard(false))
}

// handleUnlinkMax — отвязка MAX из VK (с подтверждением). Зеркало handleUnlinkTG:
// VK-идентичность отсюда не трогаем, отвязываем только MAX.
func (b *Bot) handleUnlinkMax(ctx context.Context, vkID int64, u *domain.User, confirmed bool) {
	if u.MaxID == nil {
		b.sendProfile(ctx, vkID, u)
		return
	}
	if !confirmed {
		kb := &Keyboard{Inline: true, Buttons: [][]Button{
			{TextButton("⚠️ Да, отвязать", fmt.Sprintf(`{"cmd":%q,"k":"confirm"}`, cmdUnlinkMax), ColorSecondary)},
			{TextButton("◀️ Отмена", buttonPayload(cmdProfile), ColorPrimary)},
		}}
		b.send(ctx, vkID, "Отвязать MAX от этого аккаунта?\n\n"+
			"Подписки и тариф останутся здесь, в VK. MAX-аккаунт начнёт с чистого листа "+
			"при следующем заходе в MAX-бота.", kb)
		return
	}
	if err := b.userRepo.UnlinkMax(ctx, u.ID); err != nil {
		b.log.Error("vk: unlink max", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	u.MaxID = nil
	b.send(ctx, vkID, "✅ MAX отвязан.", nil)
	b.sendProfile(ctx, vkID, u)
}

func (b *Bot) helpText(u *domain.User) string {
	base := "❓ Помощь\n\nЯ TryBerry — слежу за ценами на Wildberries и уведомляю о снижении 🍓\n\n" +
		"Как добавить товар: отправь ссылку на товар WB прямо в чат — без команд.\n" +
		"Поиск по ссылке: отправь ссылку на поисковую выдачу WB — буду следить за всей выдачей.\n\n" +
		"Кнопки внизу:\n" +
		"➕ Добавить товар / 🔎 Поиск по ссылке — как добавить\n" +
		"📋 Мои товары / 📡 Мои поиски — списки, там же отписка\n" +
		"💳 Тарифы — лимиты и цены, 🎁 Триал — попробовать поиск бесплатно\n" +
		"🎟 Промокод — активировать код, 👥 Пригласить друга — бонусные дни\n" +
		"👤 Профиль — аккаунт, уведомления, привязка\n"
	if u.TelegramID == 0 {
		base += "🔗 Привязать Telegram — связать аккаунты\n"
	}
	base += "\nВопросы — пиши @kosov_andrey (Telegram)."
	return base
}

func (b *Bot) welcomeText(u *domain.User) string {
	if u.TelegramID != 0 {
		return "Привет! Твой аккаунт связан с Telegram ✅\n\n" +
			"Отправь ссылку на товар Wildberries — начну отслеживать. " +
			"Подписки общие с Telegram, уведомления — куда настроишь (Профиль → Уведомления в TG-боте).\n\n" +
			"Кнопки внизу: «Мои товары» — список, «Помощь» — что я умею."
	}
	return "Привет! Я TryBerry — слежу за ценами на Wildberries 🍓\n\n" +
		"Отправь мне ссылку на товар — начну отслеживать и напишу, когда цена снизится.\n\n" +
		"Уже пользуешься Telegram-ботом @TryBerryBot? Нажми «Привязать Telegram» внизу — " +
		"подписки и тариф станут общими."
}

func (b *Bot) send(ctx context.Context, peerID int64, text string, kb *Keyboard) {
	if err := b.client.SendMessageKeyboard(ctx, peerID, text, kb); err != nil {
		b.log.Error("vk: send", "err", err, "peer", peerID)
	}
}

// Notify — одиночное сообщение в ЛС VK (для асинхронных уведомлений, напр. об
// успешной оплате из платёжного консьюмера).
func (b *Bot) Notify(ctx context.Context, vkID int64, text string) {
	b.send(ctx, vkID, text, nil)
}

// cutAnyPrefix — первый подошедший префикс: остаток и ok.
func cutAnyPrefix(s string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest, true
		}
	}
	return "", false
}
