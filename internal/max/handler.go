package max

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/max-messenger/max-bot-api-client-go/schemes"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// Команды кнопок (payload). Роутим по ним, а не по label.
const (
	cmdProfile     = "profile"
	cmdLinkTG      = "linktg"   // привязать Telegram (выдать код max2tg)
	cmdLinkVK      = "linkvk"   // привязать VK (выдать код max2vk)
	cmdUnlinkTG    = "unlinktg" // отвязать Telegram (k=confirm)
	cmdUnlinkVK    = "unlinkvk" // отвязать VK (k=confirm)
	cmdNotify      = "notify"    // экран выбора канала уведомлений
	cmdNotifySet   = "notifyset" // сохранить канал (k=tg|vk|max|all)
	cmdEmail       = "email"
	cmdHelp        = "help"
	cmdAdd         = "add"
	cmdList        = "list"
	cmdUntrack     = "untrack"
	cmdPTrack      = "ptrack"
	cmdPTarget     = "ptgt"
	cmdListPage    = "lpage"
	cmdLSearchPage = "lspage"
	cmdSearch      = "search"
	cmdLSearch     = "lsearch"
	cmdSTrack      = "strack"
	cmdSFSkip      = "sfskip"
	cmdSUntrack    = "suntrack"
	cmdPlans       = "plans"
	cmdPlanCard    = "plan"
	cmdBuy         = "buy"
	cmdSub         = "sub"
	cmdSubOk       = "subok"
	cmdSubCancel   = "subcancel"
	cmdSubCancelOk = "subcancelok"
	cmdTrial       = "trial"
	cmdPromo       = "promo"
	cmdRef         = "ref"
	cmdMerge       = "merge"
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

// buttonPayloadKind — payload c параметром k (например, канал у notifyset).
func buttonPayloadKind(cmd, kind string) string {
	return fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmd, kind)
}

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

// Bot — обработчик входящих апдейтов MAX. Зеркало vk.Bot.
type Bot struct {
	client          *Client
	log             *slog.Logger
	userRepo        *postgres.UserRepo
	subRepo         *postgres.SubscriptionRepo
	prodRepo        *postgres.ProductRepo
	priceRepo       *postgres.PriceHistoryRepo
	searchQueryRepo *postgres.SearchQueryRepo
	searchSubRepo   *postgres.SearchSubscriptionRepo
	promoRepo       *postgres.PromoRepo
	referralRepo    *postgres.ReferralRepo
	registry        *scraper.Registry
	resolver        *scraper.LinkResolver // короткие ссылки приложений (ozon.ru/t/…, market.yandex.ru/cc/…) — паритет с TG
	linkCodes       *redisrepo.LinkCodeStore
	rdb             *redis.Client
	botURL          string // ссылка на MAX-бота для приглашений
	chartBaseURL    string
	adminIDs        map[int64]bool

	payments  *payment.Service
	discounts *redisrepo.DiscountStore
	billing   *postgres.BillingSubscriptionRepo
}

func (b *Bot) SetPayments(svc *payment.Service) { b.payments = svc }

func (b *Bot) SetBilling(repo *postgres.BillingSubscriptionRepo) { b.billing = repo }

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
		resolver:        scraper.NewLinkResolver(0, log),
		linkCodes:       linkCodes,
		rdb:             rdb,
		botURL:          botURL,
		chartBaseURL:    chartBaseURL,
		adminIDs:        adminIDs,
		discounts:       discounts,
	}
}

func (b *Bot) isAdmin(maxID int64) bool { return b.adminIDs[maxID] }

func (b *Bot) chartURL(publicID string) string {
	if b.chartBaseURL == "" || publicID == "" {
		return ""
	}
	return b.chartBaseURL + "/p/" + publicID
}

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

// HandleUpdate — точка входа для апдейта из Kafka (декодирован DecodeUpdate).
func (b *Bot) HandleUpdate(ctx context.Context, upd schemes.UpdateInterface) {
	switch u := upd.(type) {
	case *schemes.MessageCreatedUpdate:
		// Только личные диалоги (групповые чаты игнорируем).
		if u.Message.Recipient.ChatType != schemes.DIALOG {
			return
		}
		b.handleMessage(ctx, u.Message.Sender.UserId, strings.TrimSpace(u.Message.Body.Text), "")
	case *schemes.MessageCallbackUpdate:
		b.client.AnswerCallback(ctx, u.Callback.CallbackID)
		b.handleMessage(ctx, u.Callback.User.UserId, "", u.Callback.Payload)
	case *schemes.BotStartedUpdate:
		b.handleStart(ctx, u.User.UserId, strings.TrimSpace(u.Payload))
	}
}

// handleStart — нажатие «Начать»/deeplink. Payload вида link_<код> или ref_<код>.
func (b *Bot) handleStart(ctx context.Context, maxID int64, payload string) {
	user, err := b.userRepo.UpsertMax(ctx, maxID)
	if err != nil {
		b.log.Error("max: upsert user", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if code, ok := strings.CutPrefix(payload, "link_"); ok && code != "" {
		b.handleLink(ctx, maxID, user, code)
		return
	}
	if code, ok := strings.CutPrefix(payload, "ref_"); ok && code != "" {
		b.handleRefCode(ctx, maxID, user, code)
		return
	}
	b.send(ctx, maxID, b.welcomeText(user), menuKeyboard(user))
}

// menuKeyboard — inline-меню. До привязки сверху кнопки привязки.
func menuKeyboard(u *domain.User) *Keyboard {
	var rows [][]Button
	if u.TelegramID == 0 {
		rows = append(rows, []Button{
			TextButton("🔗 Привязать Telegram", buttonPayload(cmdLinkTG), ColorPrimary),
		})
	}
	if u.VKID == nil {
		rows = append(rows, []Button{
			TextButton("🔗 Привязать VK", buttonPayload(cmdLinkVK), ColorPrimary),
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

func (b *Bot) handleMessage(ctx context.Context, maxID int64, text, payload string) {
	user, err := b.userRepo.UpsertMax(ctx, maxID)
	if err != nil {
		b.log.Error("max: upsert user", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	kb := menuKeyboard(user)

	// Короткие ссылки приложений (ozon.ru/t/…, market.yandex.ru/cc/…) → канонический
	// URL до любого разбора текста; без "://" в тексте — no-op без сети. Паритет с TG.
	text = b.resolver.ExpandInText(ctx, text)

	if strings.HasPrefix(text, "/") && b.handleSlashCommand(ctx, maxID, user, text) {
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
		b.clearSearchFSM(ctx, maxID)
		b.clearTrackFSM(ctx, maxID)
		b.clearEmailFSM(ctx, maxID)
		b.clearPromoFSM(ctx, maxID)
	} else {
		if fsm, ok := b.getEmailFSM(ctx, maxID); ok {
			metrics.MaxMessages.WithLabelValues("email_input").Inc()
			b.handleEmailInput(ctx, maxID, user, text, fsm)
			return
		}
		if fsm, ok := b.getPromoFSM(ctx, maxID); ok {
			metrics.MaxMessages.WithLabelValues("promo_input").Inc()
			b.handlePromoInput(ctx, maxID, user, text, fsm)
			return
		}
		if rest, ok := cutAnyPrefix(lower, "привязать ", "link "); ok {
			metrics.MaxMessages.WithLabelValues("link_code").Inc()
			b.handleLink(ctx, maxID, user, strings.TrimSpace(rest))
			return
		}
		if rest, ok := cutAnyPrefix(lower, "промокод ", "promo "); ok {
			metrics.MaxMessages.WithLabelValues("promo_code").Inc()
			b.clearSearchFSM(ctx, maxID)
			b.clearTrackFSM(ctx, maxID)
			b.handlePromoCode(ctx, maxID, user, strings.TrimSpace(rest))
			return
		}
		if rest, ok := cutAnyPrefix(lower, "друг ", "friend "); ok {
			metrics.MaxMessages.WithLabelValues("ref_code").Inc()
			b.clearSearchFSM(ctx, maxID)
			b.clearTrackFSM(ctx, maxID)
			b.handleRefCode(ctx, maxID, user, strings.TrimSpace(rest))
			return
		}
		if fsm, ok := b.getTrackFSM(ctx, maxID); ok {
			metrics.MaxMessages.WithLabelValues("track_threshold").Inc()
			b.handleTrackThreshold(ctx, maxID, user, text, fsm)
			return
		}
		if fsm, ok := b.getSearchFSM(ctx, maxID); ok {
			if fsm.SellerURL != "" {
				metrics.MaxMessages.WithLabelValues("seller_text_filter").Inc()
				b.handleSellerTextFilter(ctx, maxID, user, text, fsm)
			} else {
				metrics.MaxMessages.WithLabelValues("search_threshold").Inc()
				b.handleSearchThreshold(ctx, maxID, user, text, fsm)
			}
			return
		}
		if prods := b.trackableProductURLs(text); len(prods) >= 2 {
			metrics.MaxMessages.WithLabelValues("bulk_track").Inc()
			b.handleBulkTrack(ctx, maxID, prods, user)
			return
		}
		if _, err := b.registry.FindSearchByURL(text); err == nil {
			metrics.MaxMessages.WithLabelValues("search_url").Inc()
			b.startSearchTrack(ctx, maxID, user, text)
			return
		}
		if _, err := b.registry.FindByURL(text); err == nil {
			metrics.MaxMessages.WithLabelValues("track_url").Inc()
			b.handleTrack(ctx, maxID, user, text)
			return
		}
		if slug, ok := domain.SellerVanitySlug(text); ok {
			metrics.MaxMessages.WithLabelValues("seller_vanity").Inc()
			if id, err := b.registry.ResolveSellerVanity(ctx, slug); err == nil && id != "" {
				b.startSearchTrack(ctx, maxID, user, domain.RewriteSellerVanity(text, id))
			} else {
				if err != nil {
					b.log.Warn("max: resolve seller vanity", "slug", slug, "err", err)
				}
				b.send(ctx, maxID, "🏬 Не получилось открыть этот магазин по буквенной ссылке. Попробуй ссылку с числовым номером (вида /seller/250021611) — её даёт кнопка «Поделиться» на странице продавца в приложении WB.", kb)
			}
			return
		}
	}

	if p.Cmd != "" {
		metrics.MaxMessages.WithLabelValues(p.Cmd).Inc()
	} else {
		metrics.MaxMessages.WithLabelValues("other").Inc()
	}

	switch p.Cmd {
	case cmdProfile:
		b.sendProfile(ctx, maxID, user)
	case cmdNotify:
		b.sendNotifyPicker(ctx, maxID, user)
	case cmdNotifySet:
		b.setNotify(ctx, maxID, user, p.Kind)
	case cmdLinkTG:
		b.issueLinkCode(ctx, maxID, user, domain.LinkDirMax2TG)
	case cmdLinkVK:
		b.issueLinkCode(ctx, maxID, user, domain.LinkDirMax2VK)
	case cmdUnlinkTG:
		b.handleUnlink(ctx, maxID, user, "tg", p.Kind == "confirm")
	case cmdUnlinkVK:
		b.handleUnlink(ctx, maxID, user, "vk", p.Kind == "confirm")
	case cmdHelp:
		b.send(ctx, maxID, b.helpText(user), kb)
	case cmdAdd:
		b.send(ctx, maxID, "➕ Отправь мне ссылку на товар Wildberries — начну отслеживать цену.\n\n"+
			"Пример:\nhttps://www.wildberries.ru/catalog/252334498/detail.aspx", kb)
	case cmdList:
		b.handleList(ctx, maxID, user, "")
	case cmdListPage:
		b.showProductList(ctx, maxID, user, "", int(p.ID))
	case cmdUntrack:
		b.handleUntrack(ctx, maxID, user, p.ID)
	case cmdPTrack:
		b.handleProductTrigger(ctx, maxID, user, p)
	case cmdPTarget:
		b.handleProductTarget(ctx, maxID, user, p)
	case cmdSearch:
		b.send(ctx, maxID, "🔎 Поиск по ссылке\n\n"+
			"Отправь ссылку на поисковую выдачу Wildberries — буду следить за всей выдачей и напишу, когда товары подешевеют.\n\n"+
			"Как получить ссылку: на сайте WB введи запрос в поиск и скопируй адрес страницы.\n\n"+
			"Пример:\nhttps://www.wildberries.ru/catalog/0/search.aspx?search=наушники", kb)
	case cmdLSearch:
		b.handleListSearch(ctx, maxID, user, "")
	case cmdLSearchPage:
		b.showSearchList(ctx, maxID, user, "", int(p.ID))
	case cmdSTrack:
		b.handleSearchTrigger(ctx, maxID, user, p)
	case cmdSFSkip:
		b.handleSellerSkipFilter(ctx, maxID, user)
	case cmdSUntrack:
		b.handleUntrackSearch(ctx, maxID, user, p.ID)
	case cmdPlans:
		b.sendPlans(ctx, maxID, user)
	case cmdPlanCard:
		b.sendPlanCard(ctx, maxID, user, p.Kind)
	case cmdBuy:
		b.handlePlanBuy(ctx, maxID, user, p.Kind)
	case cmdSub:
		b.sendSubConsent(ctx, maxID, user, p.Kind)
	case cmdSubOk:
		b.handleSubBuy(ctx, maxID, user, p.Kind)
	case cmdSubCancel:
		b.handleSubCancelConfirm(ctx, maxID, user)
	case cmdSubCancelOk:
		b.handleSubCancel(ctx, maxID, user)
	case cmdTrial:
		b.handleTrial(ctx, maxID, user)
	case cmdPromo:
		b.promptPromo(ctx, maxID, user, p.Kind)
	case cmdEmail:
		b.promptChangeEmail(ctx, maxID, user)
	case cmdRef:
		b.sendRef(ctx, maxID, user)
	case cmdMerge:
		b.handleMergeAction(ctx, maxID, user, p.Kind)
	default:
		b.send(ctx, maxID, b.welcomeText(user), kb)
	}
}

// handleLink — гасим код привязки, выданный в TG (tg2max) или VK (vk2max):
// владение обоими аккаунтами доказано. Привязываем нашу MAX-идентичность к
// аккаунту-эмитенту кода.
func (b *Bot) handleLink(ctx context.Context, maxID int64, maxUser *domain.User, code string) {
	if b.linkCodes == nil {
		b.send(ctx, maxID, "Привязка временно недоступна, попробуй позже.", nil)
		return
	}
	dir, issuerUserID, err := b.linkCodes.Redeem(ctx, code)
	if err != nil || (dir != domain.LinkDirTG2Max && dir != domain.LinkDirVK2Max) {
		b.send(ctx, maxID, "Код не подошёл 😕 Проверь, что скопировал его целиком, "+
			"или получи новый: в Telegram/VK-боте «Привязать MAX» (код живёт 15 минут).",
			menuKeyboard(maxUser))
		return
	}

	if err := b.userRepo.LinkMax(ctx, issuerUserID, maxID); err != nil {
		if errors.Is(err, domain.ErrMaxAccountBusy) {
			b.startMergeFlow(ctx, maxID, maxUser, issuerUserID)
			return
		}
		b.log.Error("max: link", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	fresh, _ := b.userRepo.GetByMaxID(ctx, maxID)
	if fresh == nil {
		fresh = maxUser
	}
	b.send(ctx, maxID, "Готово! 🎉 Аккаунты связаны.\n\n"+
		"Теперь в Профиле (кнопка внизу) можно выбрать, куда слать уведомления.",
		menuKeyboard(fresh))
}

func (b *Bot) sendProfile(ctx context.Context, maxID int64, u *domain.User) {
	now := time.Now()
	plan := u.EffectivePlan(now)

	prod, _ := b.subRepo.CountActiveByUserID(ctx, u.ID)
	srch, _ := b.searchSubRepo.CountActiveByUserID(ctx, u.ID)

	tg := "❌ не привязан"
	if u.TelegramID != 0 {
		tg = "✅ привязан"
	}
	vk := "❌ не привязан"
	if u.VKID != nil {
		vk = "✅ привязан"
	}
	planLine := "Тариф: " + plan.Title
	if u.PlanExpiresAt != nil && !u.PlanExpired(now) {
		planLine += " (до " + u.PlanExpiresAt.Format(dateLayout) + ")"
	}

	var sb strings.Builder
	sb.WriteString("👤 Профиль\n\n")
	fmt.Fprintf(&sb, "MAX: ✅ привязан\nTelegram: %s\nVK: %s\n%s\n", tg, vk, planLine)
	fmt.Fprintf(&sb, "📦 Товаров: %d из %d · 🔎 Поисков: %d из %d\n", prod, plan.MaxProduct, srch, plan.MaxSearch)

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

	fmt.Fprintf(&sb, "🔔 Уведомления: %s\n", domain.NotifyChannelTitle(u.NotifyChannel))
	var rows [][]Button
	// Цикл канала уведомлений доступен, если есть ≥2 идентичности.
	if identityCount(u) >= 2 {
		rows = append(rows, []Button{TextButton("🔔 Уведомления: "+domain.NotifyChannelTitle(u.NotifyChannel),
			buttonPayload(cmdNotify), ColorPrimary)})
	}
	if u.TelegramID != 0 {
		rows = append(rows, []Button{TextButton("🔗 Отвязать Telegram", buttonPayload(cmdUnlinkTG), ColorSecondary)})
	} else {
		rows = append(rows, []Button{TextButton("🔗 Привязать Telegram", buttonPayload(cmdLinkTG), ColorSecondary)})
	}
	if u.VKID != nil {
		rows = append(rows, []Button{TextButton("🔗 Отвязать VK", buttonPayload(cmdUnlinkVK), ColorSecondary)})
	} else {
		rows = append(rows, []Button{TextButton("🔗 Привязать VK", buttonPayload(cmdLinkVK), ColorSecondary)})
	}
	if b.payments != nil {
		rows = append(rows, []Button{TextButton(emailLabel, buttonPayload(cmdEmail), ColorSecondary)})
	}
	if hasSub {
		rows = append(rows, []Button{TextButton("🚫 Отменить автопродление", buttonPayload(cmdSubCancel), ColorSecondary)})
	}
	rows = append(rows, []Button{TextButton("◀️ Меню", buttonPayload(""), ColorSecondary)})
	b.send(ctx, maxID, sb.String(), &Keyboard{Buttons: rows})
}

// identityCount — сколько идентичностей привязано (для доступности цикла каналов).
func identityCount(u *domain.User) int {
	n := 1 // MAX всегда есть в этом боте
	if u.TelegramID != 0 {
		n++
	}
	if u.VKID != nil {
		n++
	}
	return n
}

// maxNotifyCycle — доступные каналы уведомлений: MAX + привязанные + «везде».
func maxNotifyCycle(u *domain.User) []string {
	cycle := []string{domain.NotifyMax}
	if u.TelegramID != 0 {
		cycle = append(cycle, domain.NotifyTG)
	}
	if u.VKID != nil {
		cycle = append(cycle, domain.NotifyVK)
	}
	return append(cycle, domain.NotifyAll)
}

// sendNotifyPicker — экран выбора канала уведомлений (радио-список вместо
// слепого цикла): ● отмечает текущий, нажатие сохраняет сразу и присылает
// обновлённый экран, «Профиль» возвращает назад. Доступно при ≥2 привязках.
func (b *Bot) sendNotifyPicker(ctx context.Context, maxID int64, u *domain.User) {
	if identityCount(u) < 2 {
		b.sendProfile(ctx, maxID, u)
		return
	}
	text := "🔔 Куда присылать уведомления?\n\n" +
		"Пуши о ценах, дайджесты и сервисные сообщения пойдут в выбранный канал. " +
		"Нажми вариант — сохранится сразу."
	var rows [][]Button
	for _, ch := range maxNotifyCycle(u) {
		mark := "○"
		if ch == u.NotifyChannel {
			mark = "●"
		}
		rows = append(rows, []Button{TextButton(mark+" "+domain.NotifyChannelTitle(ch),
			buttonPayloadKind(cmdNotifySet, ch), ColorSecondary)})
	}
	rows = append(rows, []Button{TextButton("👤 Профиль", buttonPayload(cmdProfile), ColorSecondary)})
	b.send(ctx, maxID, text, &Keyboard{Buttons: rows})
}

// setNotify — сохранить выбранный канал (только из доступных) и показать
// обновлённый экран выбора.
func (b *Bot) setNotify(ctx context.Context, maxID int64, u *domain.User, ch string) {
	valid := false
	for _, c := range maxNotifyCycle(u) {
		if c == ch {
			valid = true
			break
		}
	}
	if valid && ch != u.NotifyChannel {
		if err := b.userRepo.SetNotifyChannel(ctx, u.ID, ch); err != nil {
			b.log.Error("max: set notify channel", "err", err)
			b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
			return
		}
		u.NotifyChannel = ch
	}
	b.sendNotifyPicker(ctx, maxID, u)
}

// issueLinkCode — выдать код привязки (направление max2tg / max2vk).
func (b *Bot) issueLinkCode(ctx context.Context, maxID int64, u *domain.User, dir string) {
	target := "Telegram"
	botMention := "Telegram-бота @TryBerryBot (t.me/TryBerryBot)"
	already := u.TelegramID != 0
	if dir == domain.LinkDirMax2VK {
		target = "VK"
		botMention = "VK-бота TryBerry"
		already = u.VKID != nil
	}
	if already {
		b.send(ctx, maxID, "Твой аккаунт уже связан с "+target+" ✅\n\n"+
			"Сменить привязку: сначала «Отвязать "+target+"» в Профиле, потом привязать заново.",
			menuKeyboard(u))
		return
	}
	if b.linkCodes == nil {
		b.send(ctx, maxID, "Привязка временно недоступна, попробуй позже.", nil)
		return
	}
	code, err := b.linkCodes.Issue(ctx, u.ID, dir)
	if err != nil {
		if errors.Is(err, domain.ErrLinkCodeRateLimited) {
			b.send(ctx, maxID, "⏳ Код уже выдан — подожди минуту и попробуй снова, если не успел его использовать.", nil)
			return
		}
		b.log.Error("max: issue link code", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	ttlMin := int(domain.LinkCodeTTL.Minutes())
	b.send(ctx, maxID, fmt.Sprintf(
		"🔗 Привязка %s\n\n"+
			"1. Открой %s\n"+
			"2. Отправь ему сообщение:\n\nпривязать %s\n\n"+
			"Код действует %d минут и работает один раз. Никому его не пересылай — "+
			"это ключ к твоему аккаунту.",
		target, botMention, code, ttlMin), menuKeyboard(u))
}

// handleUnlink — отвязка Telegram/VK из MAX (с подтверждением).
func (b *Bot) handleUnlink(ctx context.Context, maxID int64, u *domain.User, what string, confirmed bool) {
	target := "Telegram"
	linked := u.TelegramID != 0
	if what == "vk" {
		target = "VK"
		linked = u.VKID != nil
	}
	if !linked {
		b.sendProfile(ctx, maxID, u)
		return
	}
	cmd := cmdUnlinkTG
	if what == "vk" {
		cmd = cmdUnlinkVK
	}
	if !confirmed {
		kb := &Keyboard{Buttons: [][]Button{
			{TextButton("⚠️ Да, отвязать", fmt.Sprintf(`{"cmd":%q,"k":"confirm"}`, cmd), ColorSecondary)},
			{TextButton("◀️ Отмена", buttonPayload(cmdProfile), ColorPrimary)},
		}}
		b.send(ctx, maxID, "Отвязать "+target+" от этого аккаунта?\n\n"+
			"Подписки и тариф останутся здесь, в MAX. "+target+"-аккаунт начнёт с чистого листа.", kb)
		return
	}
	var err error
	if what == "vk" {
		err = b.userRepo.UnlinkVK(ctx, u.ID)
	} else {
		err = b.userRepo.UnlinkTG(ctx, u.ID)
	}
	if err != nil {
		b.log.Error("max: unlink", "what", what, "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if what == "vk" {
		u.VKID = nil
	} else {
		u.TelegramID = 0
	}
	b.send(ctx, maxID, "✅ "+target+" отвязан.", menuKeyboard(u))
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
	if u.TelegramID == 0 || u.VKID == nil {
		base += "🔗 Привязать Telegram/VK — связать аккаунты\n"
	}
	base += "\nВопросы — пиши @kosov_andrey (Telegram)."
	return base
}

func (b *Bot) welcomeText(u *domain.User) string {
	if u.TelegramID != 0 || u.VKID != nil {
		return "Привет! Твой аккаунт связан ✅\n\n" +
			"Отправь ссылку на товар Wildberries — начну отслеживать. " +
			"Подписки общие, уведомления — куда настроишь (Профиль → Уведомления).\n\n" +
			"Кнопки внизу: «Мои товары» — список, «Помощь» — что я умею."
	}
	return "Привет! Я TryBerry — слежу за ценами на Wildberries 🍓\n\n" +
		"Отправь мне ссылку на товар — начну отслеживать и напишу, когда цена снизится.\n\n" +
		"Уже пользуешься Telegram- или VK-ботом TryBerry? Нажми «Привязать» внизу — " +
		"подписки и тариф станут общими."
}

func (b *Bot) send(ctx context.Context, userID int64, text string, kb *Keyboard) {
	if err := b.client.SendMessageKeyboard(ctx, userID, text, kb); err != nil {
		b.log.Error("max: send", "err", err, "user", userID)
	}
}

// Notify — одиночное сообщение в ЛС MAX (асинхронные уведомления, напр. оплата).
func (b *Bot) Notify(ctx context.Context, maxID int64, text string) {
	b.send(ctx, maxID, text, nil)
}

func cutAnyPrefix(s string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest, true
		}
	}
	return "", false
}
