package domain

import (
	"testing"
	"time"
)

func TestPlusPlanByName(t *testing.T) {
	p, ok := PlanByName("pro_plus_s30_p150")
	if !ok {
		t.Fatal("pro_plus_s30_p150 not parsed")
	}
	if p.MaxSearch != 30 || p.MaxProduct != 150 {
		t.Errorf("limits = %d/%d", p.MaxSearch, p.MaxProduct)
	}
	if p.PriceRub != 499+10*35+10*30+1*50 {
		t.Errorf("price = %d", p.PriceRub)
	}
	if p.SubPriceRub != 1199-60 {
		t.Errorf("sub price = %d", p.SubPriceRub)
	}
	if p.Interval != Plans["pro"].Interval || p.SearchCooldown != Plans["pro"].SearchCooldown {
		t.Errorf("intervals not inherited from pro")
	}
	if p.BundleWindow() != Plans["pro"].BundleWindow() {
		t.Errorf("bundle window = %v", p.BundleWindow())
	}

	r, ok := PlanByName("reseller_pro_plus_s5_p20")
	if !ok {
		t.Fatal("reseller_pro_plus_s5_p20 not parsed")
	}
	if r.PriceRub != 1990+2*500+1*150 || r.Interval != time.Minute {
		t.Errorf("reseller+: price=%d interval=%v", r.PriceRub, r.Interval)
	}
	if !IsResellerPlan(r.Name) || r.BundleWindow() != 0 {
		t.Errorf("reseller+ must ride the reseller lane")
	}
	if IsResellerPlan("pro_plus_s15_p100") {
		t.Errorf("pro+ is not reseller")
	}
}

func TestPlusPlanByNameRejects(t *testing.T) {
	for _, name := range []string{
		"pro_plus_s10_p100",        // = база, покупается как pro
		"pro_plus_s12_p100",        // вне сетки
		"pro_plus_s5_p100",         // ниже базы
		"pro_plus_s55_p100",        // выше потолка
		"pro_plus_s15_p550",        // выше потолка
		"pro_plus_s015_p100",       // неканонично
		"pro_plus_s15",             // нет товаров
		"pro_plus_s15_p100_x",      // хвост
		"reseller_pro_plus_s2_p15", // ниже базы
		"lite_plus_s5_p30",         // нет линейки
	} {
		if _, ok := PlanByName(name); ok {
			t.Errorf("%s: must be rejected", name)
		}
	}
}

func TestPlusEffectivePlanExpiry(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	if got := EffectivePlanFor("pro_plus_s20_p100", &future, now); got.MaxSearch != 20 {
		t.Errorf("active pro+: MaxSearch=%d", got.MaxSearch)
	}
	if got := EffectivePlanFor("pro_plus_s20_p100", &past, now); got.Name != "free" {
		t.Errorf("expired pro+: got %s", got.Name)
	}
}

func TestPlusStep(t *testing.T) {
	cases := []struct {
		from   string
		ds, dp int
		want   string
	}{
		{"pro_plus_s15_p100", 1, 0, "pro_plus_s20_p100"},
		{"pro_plus_s15_p100", -1, 0, "pro_plus_s10_p100"},
		{"pro_plus_s10_p100", -1, -1, "pro_plus_s10_p100"}, // пол базы
		{"pro_plus_s50_p500", 1, 1, "pro_plus_s50_p500"},   // потолок
		{"reseller_pro_plus_s4_p15", 0, 1, "reseller_pro_plus_s4_p20"},
	}
	for _, c := range cases {
		got, ok := PlusStep(c.from, c.ds, c.dp)
		if !ok || got != c.want {
			t.Errorf("PlusStep(%s,%d,%d) = %s,%v; want %s", c.from, c.ds, c.dp, got, ok, c.want)
		}
	}
	if _, ok := PlusStep("pro", 1, 0); ok {
		t.Errorf("non-plus name must fail")
	}
}

func TestPlusPayNameAndDefaults(t *testing.T) {
	if n, _ := PlusPayName("pro_plus_s10_p100"); n != "pro" {
		t.Errorf("base point must pay as pro, got %s", n)
	}
	if n, _ := PlusPayName("pro_plus_s20_p100"); n != "pro_plus_s20_p100" {
		t.Errorf("got %s", n)
	}
	for _, pb := range PlusBases {
		if _, ok := PlanByName(pb.DefaultName()); !ok {
			t.Errorf("%s: default %s not purchasable", pb.Key, pb.DefaultName())
		}
		if _, ok := Plans[pb.Base]; !ok {
			t.Errorf("%s: base %s missing", pb.Key, pb.Base)
		}
	}
	if txt, ok := PlusConfigText("pro_plus_s20_p100", "", ""); !ok || txt == "" {
		t.Errorf("config text empty")
	}
}

func TestPlusTiers(t *testing.T) {
	pro, _ := PlusBaseByKey("pro_plus")
	for _, c := range []struct{ s, price, savings int }{
		{10, 499, 0}, {15, 674, 0}, {20, 849, 0}, {25, 999, 25}, {30, 1149, 50}, {40, 1399, 150}, {50, 1649, 250},
	} {
		if got := pro.Price(c.s, 100); got != c.price {
			t.Errorf("pro+ s%d: price %d, want %d", c.s, got, c.price)
		}
		if got := pro.SearchSavings(c.s); got != c.savings {
			t.Errorf("pro+ s%d: savings %d, want %d", c.s, got, c.savings)
		}
	}
	if got := pro.TierLadder(); got != "11–20 по 35 ₽ · 21–30 по 30 ₽ · 31–50 по 25 ₽" {
		t.Errorf("ladder = %q", got)
	}
	for s, want := range map[int]string{
		10: "➕ С 21-го поиска — по 30 ₽",
		20: "➕ С 21-го поиска — по 30 ₽",
		25: "➕ С 31-го поиска — по 25 ₽",
		30: "➕ С 31-го поиска — по 25 ₽",
		35: "", // последняя ступень
		50: "",
	} {
		if got := pro.NextSearchHint(s); got != want {
			t.Errorf("hint@%d = %q, want %q", s, got, want)
		}
	}
	if pro.FromRub() != 549 {
		t.Errorf("pro+ from = %d", pro.FromRub())
	}

	rp, _ := PlusBaseByKey("reseller_pro_plus")
	if got := rp.Price(10, 15); got != 1990+2*500+2*450+3*400 {
		t.Errorf("reseller+ s10 = %d", got)
	}
	if got := rp.TierLadder(); got != "4–5 по 500 ₽ · 6–7 по 450 ₽ · 8–10 по 400 ₽" {
		t.Errorf("reseller ladder = %q", got)
	}
}

// Инварианты лесенок: возрастающие границы, последняя = потолок, границы
// кратны шагу от базы (шаг не перескакивает ступень), цена ступеней падает.
func TestPlusTierInvariants(t *testing.T) {
	for _, pb := range PlusBases {
		b := pb.BasePlan()
		prevUp, prevRub := b.MaxSearch, 1<<30
		for _, tr := range pb.SearchTiers {
			if tr.UpTo <= prevUp || (tr.UpTo-b.MaxSearch)%pb.SearchStep != 0 || tr.Rub >= prevRub {
				t.Errorf("%s: bad tier %+v", pb.Key, tr)
			}
			prevUp, prevRub = tr.UpTo, tr.Rub
		}
		if prevUp != pb.MaxSearch {
			t.Errorf("%s: last tier %d != MaxSearch %d", pb.Key, prevUp, pb.MaxSearch)
		}
	}
}
