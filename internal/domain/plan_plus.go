package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// «Плюс»-тарифы (Pro+, Reseller Pro+) — надстройка над базовым планом: та же
// частота проверки, но больше поисков/товаров. Юзер собирает их сам в
// конфигураторе бота. Параметры зашиты прямо в имя плана
// («pro_plus_s30_p150»), поэтому users.plan / payments.plan /
// billing_subscriptions.plan хранят его как обычный план, а PlanByName
// собирает Plan на лету — планировщик, воркеры, reconciler, оплата и
// автопродление работают без отдельной логики и миграций.
//
// Конфигуратор позволяет только РАСШИРЯТЬ базу (меньше базы — это Lite/Start).
// ЦЕНЫ ШАГОВ МЕНЯЮТСЯ ЗДЕСЬ. Уже купленные «плюс»-планы пересчитаются по новой
// формуле при следующем автопродлении.

// PlusBase — описание «плюс»-линейки над базовым планом.
type PlusBase struct {
	Key   string // префикс имени плана: «pro_plus»
	Title string // «Pro+»
	Base  string // базовый план: лимиты-минимум, цена-база, интервалы

	SearchStep     int // шаг конфигуратора по поискам
	SearchStepRub  int // цена шага по поискам, ₽/мес
	ProductStep    int // шаг по товарам
	ProductStepRub int // цена шага по товарам, ₽/мес
	MaxSearch      int // потолок конфигуратора
	MaxProduct     int
}

// PlusBases — линейки «плюс»-тарифов (порядок = порядок на экране выбора).
//
// Pro+: Pro = 10 поисков за 499 ₽ (~50 ₽/поиск); сверх базы — 30 ₽/поиск
// (оптом дешевле, но поиск — самый дорогой по нагрузке ресурс, ниже не
// опускаем). Товары дешёвые — 1 ₽/товар. 20 поисков = 799 ₽, 30 = 1099 ₽.
//
// Reseller Pro+: минутная проверка в 15 раз дороже 15-минутной. Маржинальная
// цена внутри линейки: Start→Pro = +2 поиска +10 товаров за 1000 ₽ — отсюда
// 450 ₽/поиск и 150 ₽ за 5 товаров.
var PlusBases = []PlusBase{
	{Key: "pro_plus", Title: "Pro+", Base: "pro",
		SearchStep: 5, SearchStepRub: 150, ProductStep: 50, ProductStepRub: 50,
		MaxSearch: 50, MaxProduct: 500},
	{Key: "reseller_pro_plus", Title: "Reseller Pro+", Base: "reseller_pro",
		SearchStep: 1, SearchStepRub: 450, ProductStep: 5, ProductStepRub: 150,
		MaxSearch: 10, MaxProduct: 50},
}

// PlusBaseByKey — линейка по ключу (ok=false, если нет).
func PlusBaseByKey(key string) (PlusBase, bool) {
	for _, pb := range PlusBases {
		if pb.Key == key {
			return pb, true
		}
	}
	return PlusBase{}, false
}

// BasePlan — базовый план линейки.
func (pb PlusBase) BasePlan() Plan { return Plans[pb.Base] }

// Name — каноническое имя «плюс»-плана с заданными лимитами.
func (pb PlusBase) Name(search, product int) string {
	return fmt.Sprintf("%s_s%d_p%d", pb.Key, search, product)
}

// DefaultName — стартовая точка конфигуратора: база + один шаг по поискам
// (за поисками и приходят — «мало 10 поисков»).
func (pb PlusBase) DefaultName() string {
	b := pb.BasePlan()
	return pb.Name(b.MaxSearch+pb.SearchStep, b.MaxProduct)
}

// FromRub — минимальная цена линейки (база + самый дешёвый шаг) для «от N ₽».
func (pb PlusBase) FromRub() int {
	return pb.BasePlan().PriceRub + min(pb.SearchStepRub, pb.ProductStepRub)
}

// Price — цена разовой оплаты, ₽/мес.
func (pb PlusBase) Price(search, product int) int {
	b := pb.BasePlan()
	return b.PriceRub +
		(search-b.MaxSearch)/pb.SearchStep*pb.SearchStepRub +
		(product-b.MaxProduct)/pb.ProductStep*pb.ProductStepRub
}

// valid — лимиты в сетке шагов, не ниже базы и не выше потолка.
func (pb PlusBase) valid(search, product int) bool {
	b := pb.BasePlan()
	return search >= b.MaxSearch && search <= pb.MaxSearch &&
		product >= b.MaxProduct && product <= pb.MaxProduct &&
		(search-b.MaxSearch)%pb.SearchStep == 0 &&
		(product-b.MaxProduct)%pb.ProductStep == 0
}

// IsBase — конфигурация совпадает с базовым планом (ничего не добавлено).
func (pb PlusBase) IsBase(search, product int) bool {
	b := pb.BasePlan()
	return search == b.MaxSearch && product == b.MaxProduct
}

// ParsePlusConfig — разбор имени «плюс»-конфигурации. В отличие от
// PlanByName принимает и точку «= база» (конфигуратор может туда прийти
// кнопкой «−»), но не неканоничное/вне сетки.
func ParsePlusConfig(name string) (pb PlusBase, search, product int, ok bool) {
	for _, cand := range PlusBases {
		rest, found := strings.CutPrefix(name, cand.Key+"_s")
		if !found {
			continue
		}
		sStr, pStr, found := strings.Cut(rest, "_p")
		if !found {
			return PlusBase{}, 0, 0, false
		}
		s, err1 := strconv.Atoi(sStr)
		p, err2 := strconv.Atoi(pStr)
		if err1 != nil || err2 != nil || !cand.valid(s, p) || cand.Name(s, p) != name {
			return PlusBase{}, 0, 0, false
		}
		return cand, s, p, true
	}
	return PlusBase{}, 0, 0, false
}

// plusPlanByName — Plan для «плюс»-имени. ok=false для точки «= база»:
// её покупают как обычный базовый план.
func plusPlanByName(name string) (Plan, bool) {
	pb, s, p, ok := ParsePlusConfig(name)
	if !ok || pb.IsBase(s, p) {
		return Plan{}, false
	}
	plan := pb.BasePlan()
	price := pb.Price(s, p)
	plan.Name = name
	plan.Title = fmt.Sprintf("%s · %d %s · %d %s",
		pb.Title, s, pluralRu(s, "поиск", "поиска", "поисков"), p, pluralRu(p, "товар", "товара", "товаров"))
	plan.MaxSearch = s
	plan.MaxProduct = p
	plan.PriceRub = price
	plan.SubPriceRub = 0
	if b := pb.BasePlan(); b.SubPriceRub > 0 {
		plan.SubPriceRub = plusSubPriceRub(price)
	}
	return plan, true
}

// plusSubPriceRub — цена автопродления: −5% к разовой, округлено до 10 ₽
// (как у базовых: 499→479, 1990→1890).
func plusSubPriceRub(price int) int {
	return price - (price*5+500)/1000*10
}

// IsPlusPlan — «плюс»-план (Pro+/Reseller Pro+) по имени.
func IsPlusPlan(name string) bool {
	_, _, _, ok := ParsePlusConfig(name)
	return ok
}

// PlusStep — соседняя конфигурация: dSearch/dProduct шагов (±1) от текущей,
// с упором в пол базы и потолок. Невалидное имя → DefaultName первой линейки
// не делаем: возвращаем ok=false, вызывающий покажет экран выбора.
func PlusStep(name string, dSearch, dProduct int) (string, bool) {
	pb, s, p, ok := ParsePlusConfig(name)
	if !ok {
		return "", false
	}
	b := pb.BasePlan()
	s = clamp(s+dSearch*pb.SearchStep, b.MaxSearch, pb.MaxSearch)
	p = clamp(p+dProduct*pb.ProductStep, b.MaxProduct, pb.MaxProduct)
	return pb.Name(s, p), true
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// PlusPickerText — экран «Нужно больше?»: выбор линейки. b0/b1 — обёртка
// жирного (TG: <b></b>, VK/MAX: пусто), как в TrialPlanActiveText.
func PlusPickerText(b0, b1 string) string {
	var sb strings.Builder
	sb.WriteString("➕ " + b0 + "Нужно больше поисков или товаров?" + b1 + "\n\n" +
		"Собери свой тариф: та же частота проверки, что у базового, но лимиты — сколько нужно. " +
		"Цена посчитается сразу.\n\n")
	for _, pb := range PlusBases {
		b := pb.BasePlan()
		fmt.Fprintf(&sb, "▫️ %s%s%s — на основе %s, проверка %s. От %d ₽/мес, до %d поисков и %d товаров.\n",
			b0, pb.Title, b1, b.Title, IntervalPhrase(b.Interval), pb.FromRub(), pb.MaxSearch, pb.MaxProduct)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// PlusConfigText — экран конфигуратора для конфигурации name (ok=false —
// невалидное имя). Точка «= база» подсказывает, что это обычный базовый план.
func PlusConfigText(name, b0, b1 string) (string, bool) {
	pb, s, p, ok := ParsePlusConfig(name)
	if !ok {
		return "", false
	}
	b := pb.BasePlan()
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚙️ %sСобери свой %s%s\n", b0, pb.Title, b1)
	fmt.Fprintf(&sb, "Всё как в %s — проверка %s, — но больше лимитов.\n\n", b.Title, IntervalPhrase(b.Interval))
	fmt.Fprintf(&sb, "🔎 Поиск-подписок: %s%d%s  (+%d — %d ₽)\n", b0, s, b1, pb.SearchStep, pb.SearchStepRub)
	fmt.Fprintf(&sb, "📦 Товаров: %s%d%s  (+%d — %d ₽)\n\n", b0, p, b1, pb.ProductStep, pb.ProductStepRub)
	if pb.IsBase(s, p) {
		fmt.Fprintf(&sb, "Это обычный тариф %s%s%s — %d ₽/мес. Добавь поисков или товаров кнопками ниже.",
			b0, b.Title, b1, b.PriceRub)
		return sb.String(), true
	}
	plan, _ := plusPlanByName(name)
	fmt.Fprintf(&sb, "💳 Итого: %s%d ₽/мес%s", b0, plan.PriceRub, b1)
	if plan.SubPriceRub > 0 {
		fmt.Fprintf(&sb, " · с автопродлением %d ₽", plan.SubPriceRub)
	}
	return sb.String(), true
}

// PlusPayName — куда ведёт «Дальше» из конфигуратора: сам «плюс»-план либо,
// на точке «= база», обычный базовый план.
func PlusPayName(name string) (string, bool) {
	pb, s, p, ok := ParsePlusConfig(name)
	if !ok {
		return "", false
	}
	if pb.IsBase(s, p) {
		return pb.Base, true
	}
	return name, true
}

// PlusKeyForBase — линейка, расширяющая план base (для кнопки «Нужно больше»
// на карточке Pro/Reseller Pro). ok=false, если такой нет.
func PlusKeyForBase(base string) (PlusBase, bool) {
	for _, pb := range PlusBases {
		if pb.Base == base {
			return pb, true
		}
	}
	return PlusBase{}, false
}

// PlanMetricLabel — имя плана для метрик: «плюс»-конфигурации сворачиваем в
// ключ линейки (pro_plus), чтобы не плодить кардинальность лейблов.
func PlanMetricLabel(name string) string {
	if pb, _, _, ok := ParsePlusConfig(name); ok {
		return pb.Key
	}
	return name
}
