package postgres

import "testing"

func TestPickMergeBillingWinner(t *testing.T) {
	const keptID, absorbedID = int64(1), int64(2)

	tests := []struct {
		name string
		subs []mergeLiveSub
		plan string
		want int64 // ID победителя, -1 = «нет»
	}{
		{
			name: "обе active, план absorbed совпал с выбранным — гасим kept",
			subs: []mergeLiveSub{
				{ID: 10, Plan: "lite", Status: "active", UserID: keptID},
				{ID: 20, Plan: "pro", Status: "active", UserID: absorbedID},
			},
			plan: "pro",
			want: 20,
		},
		{
			name: "обе active, план kept совпал — kept побеждает",
			subs: []mergeLiveSub{
				{ID: 10, Plan: "pro", Status: "active", UserID: keptID},
				{ID: 20, Plan: "lite", Status: "active", UserID: absorbedID},
			},
			plan: "pro",
			want: 10,
		},
		{
			name: "обе active, оба плана совпали — kept по тай-брейку",
			subs: []mergeLiveSub{
				{ID: 20, Plan: "pro", Status: "active", UserID: absorbedID},
				{ID: 10, Plan: "pro", Status: "active", UserID: keptID},
			},
			plan: "pro",
			want: 10,
		},
		{
			name: "kept active + чужая past_due того же плана — active важнее",
			subs: []mergeLiveSub{
				{ID: 20, Plan: "pro", Status: "past_due", UserID: absorbedID},
				{ID: 10, Plan: "pro", Status: "active", UserID: keptID},
			},
			plan: "pro",
			want: 10,
		},
		{
			name: "past_due absorbed совпала с выбранным планом — план важнее статуса",
			subs: []mergeLiveSub{
				{ID: 10, Plan: "lite", Status: "active", UserID: keptID},
				{ID: 20, Plan: "pro", Status: "past_due", UserID: absorbedID},
			},
			plan: "pro",
			want: 20,
		},
		{
			name: "у одного юзера active+past_due (гонка апгрейда) — гасим past_due",
			subs: []mergeLiveSub{
				{ID: 11, Plan: "pro", Status: "active", UserID: keptID},
				{ID: 12, Plan: "pro", Status: "past_due", UserID: keptID},
				{ID: 20, Plan: "lite", Status: "past_due", UserID: absorbedID},
			},
			plan: "pro",
			want: 11,
		},
		{
			name: "пусто — -1",
			subs: nil,
			plan: "pro",
			want: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickMergeBillingWinner(tt.subs, tt.plan, keptID)
			var gotID int64 = -1
			if got >= 0 {
				gotID = tt.subs[got].ID
			}
			if gotID != tt.want {
				t.Errorf("победитель id=%d, ожидали %d", gotID, tt.want)
			}
		})
	}
}
