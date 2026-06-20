package scraper

import "context"

// SearchToken — данные, которыми браузер аутентифицирует запрос к WB-поиску:
// строка cookie (содержит x_wbaas_token, полученный после решения PoW-челленджа
// wbaas) и User-Agent, под которым токен был выписан.
type SearchToken struct {
	Cookie    string // полная cookie-строка из браузера
	UserAgent string // UA, которым получен токен (может быть пустым → дефолт)
	Slot      int    // индекс слота в пуле; -1 для статического/legacy-токена
}

// Valid — есть ли хоть какой-то токен.
func (t SearchToken) Valid() bool { return t.Cookie != "" }

// TokenProvider отдаёт актуальный токен WB-поиска и принимает обратную связь об
// его (не)работоспособности. Реализации: пул в Redis (прод, round-robin),
// статическая (тесты/локально), функция-адаптер.
type TokenProvider interface {
	Token(ctx context.Context) (SearchToken, error)
	// MarkBad — сигнал, что запрос с токеном слота получил 429 (протух/лимит).
	// Реализация сама решает, когда вывести слот из ротации (напр. после N подряд).
	MarkBad(ctx context.Context, slot int)
	// MarkGood — сигнал успешного запроса (сбрасывает счётчик подряд идущих 429).
	MarkGood(ctx context.Context, slot int)
}

// StaticTokenProvider — фиксированный токен (тесты, локальный запуск).
type StaticTokenProvider struct{ T SearchToken }

func (s StaticTokenProvider) Token(context.Context) (SearchToken, error) { return s.T, nil }
func (s StaticTokenProvider) MarkBad(context.Context, int)               {}
func (s StaticTokenProvider) MarkGood(context.Context, int)              {}
