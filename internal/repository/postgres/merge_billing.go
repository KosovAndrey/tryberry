package postgres

// mergeLiveSub — «живая» (заряжаемая чарджером) биллинг-подписка при слиянии
// аккаунтов: status active или past_due.
type mergeLiveSub struct {
	ID     int64
	Plan   string
	Status string
	UserID int64
}

// pickMergeBillingWinner выбирает, какая из живых биллинг-подписок переживёт
// слияние (возвращает индекс в subs, -1 для пустого среза). Приоритеты по
// убыванию: план совпал с итоговым тарифом слияния > статус active (платёж
// проходит, а не в dunning) > принадлежит kept. Тай-брейк — порядок subs
// (ожидается created_at DESC, т.е. свежее побеждает).
func pickMergeBillingWinner(subs []mergeLiveSub, plan string, keptID int64) int {
	best, bestScore := -1, -1
	for i, s := range subs {
		score := 0
		if s.Plan == plan {
			score += 4
		}
		if s.Status == "active" {
			score += 2
		}
		if s.UserID == keptID {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}
