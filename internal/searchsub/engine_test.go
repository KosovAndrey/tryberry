package searchsub

import "testing"

func TestDecide_BelowTarget_FirstTime(t *testing.T) {
	r := Rule{Kind: Below, TargetKopecks: 6_000_000} // 60 000 ₽
	cases := []struct {
		name    string
		current int64
		want    bool
	}{
		{"выше порога", 6_500_000, false},
		{"ровно порог", 6_000_000, true},
		{"ниже порога", 5_900_000, true},
	}
	for _, c := range cases {
		st := ProductState{CurrentKopecks: c.current, BaselineKopecks: 7_000_000}
		if got := Decide(r, st); got != c.want {
			t.Errorf("%s: Decide=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecide_AnyDrop_FirstTime(t *testing.T) {
	r := Rule{Kind: AnyDrop}
	base := int64(5_000_000)
	if Decide(r, ProductState{CurrentKopecks: base, BaselineKopecks: base}) {
		t.Error("равно baseline — не должно срабатывать")
	}
	if !Decide(r, ProductState{CurrentKopecks: base - 1, BaselineKopecks: base}) {
		t.Error("ниже baseline на копейку — должно срабатывать")
	}
	if Decide(r, ProductState{CurrentKopecks: base + 100, BaselineKopecks: base}) {
		t.Error("выше baseline — не должно срабатывать")
	}
	if Decide(r, ProductState{CurrentKopecks: 100, BaselineKopecks: 0}) {
		t.Error("без baseline (0) — не должно срабатывать")
	}
}

func TestDecide_DiscountPct_FirstTime(t *testing.T) {
	r := Rule{Kind: Discount, DiscountPct: 20}
	base := int64(10_000_000) // 100 000 ₽; порог 20% → 80 000 ₽ = 8 000 000
	if got := DiscountThreshold(base, 20); got != 8_000_000 {
		t.Fatalf("threshold = %d, want 8000000", got)
	}
	if Decide(r, ProductState{CurrentKopecks: 8_100_000, BaselineKopecks: base}) {
		t.Error("скидка <20% — не должно срабатывать")
	}
	if !Decide(r, ProductState{CurrentKopecks: 8_000_000, BaselineKopecks: base}) {
		t.Error("скидка ровно 20% — должно срабатывать")
	}
	if !Decide(r, ProductState{CurrentKopecks: 7_500_000, BaselineKopecks: base}) {
		t.Error("скидка 25% — должно срабатывать")
	}
}

func TestDecide_Repeat_OnlyOnFurtherDrop(t *testing.T) {
	// После первого уведомления тип триггера уже не важен — важна только
	// цена последнего уведомления.
	for _, kind := range []TriggerKind{Below, AnyDrop, Discount} {
		r := Rule{Kind: kind, TargetKopecks: 9_000_000, DiscountPct: 5}
		st := ProductState{
			BaselineKopecks:     10_000_000,
			LastNotifiedKopecks: 6_000_000,
			HasNotified:         true,
		}
		// Цена не изменилась — молчим.
		st.CurrentKopecks = 6_000_000
		if Decide(r, st) {
			t.Errorf("%s: цена == последней уведомлённой — не должно слать", kind)
		}
		// Цена подросла, но всё ещё под target — всё равно молчим.
		st.CurrentKopecks = 6_500_000
		if Decide(r, st) {
			t.Errorf("%s: цена выросла — не должно слать", kind)
		}
		// Цена упала ещё ниже — шлём.
		st.CurrentKopecks = 5_999_000
		if !Decide(r, st) {
			t.Errorf("%s: цена упала ниже последней уведомлённой — должно слать", kind)
		}
	}
}

func TestDiscountThreshold_Guards(t *testing.T) {
	if DiscountThreshold(0, 20) != 0 {
		t.Error("baseline=0 → 0")
	}
	if DiscountThreshold(1000, 0) != 0 {
		t.Error("pct=0 → 0")
	}
	if DiscountThreshold(1000, 100) != 0 {
		t.Error("pct=100 (вне 1..99) → 0")
	}
}

func TestEvaluate_Batch(t *testing.T) {
	r := Rule{Kind: Below, TargetKopecks: 5_000_000}
	products := []ProductState{
		{ProductID: 1, CurrentKopecks: 4_000_000, BaselineKopecks: 6_000_000}, // hit
		{ProductID: 2, CurrentKopecks: 5_500_000, BaselineKopecks: 6_000_000}, // miss
		{ProductID: 3, CurrentKopecks: 5_000_000, BaselineKopecks: 6_000_000}, // hit (ровно)
		{ProductID: 4, CurrentKopecks: 9_000_000, BaselineKopecks: 9_000_000}, // miss
	}
	hits := Evaluate(r, products)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].ProductID != 1 || hits[1].ProductID != 3 {
		t.Errorf("неверные товары в батче: %+v", hits)
	}
}

func TestEvaluate_EmptyIsNil(t *testing.T) {
	r := Rule{Kind: Below, TargetKopecks: 1}
	hits := Evaluate(r, []ProductState{{ProductID: 1, CurrentKopecks: 999}})
	if hits != nil {
		t.Errorf("ожидался nil при отсутствии срабатываний, got %+v", hits)
	}
}

func TestDecide_UnknownKind(t *testing.T) {
	if Decide(Rule{Kind: "bogus"}, ProductState{CurrentKopecks: 1, BaselineKopecks: 100}) {
		t.Error("неизвестный тип триггера не должен срабатывать")
	}
}
