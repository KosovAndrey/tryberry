package scraper

import "context"

// SearchToken — данные, которыми браузер аутентифицирует запрос к WB-поиску:
// строка cookie (содержит x_wbaas_token, полученный после решения PoW-челленджа
// wbaas) и User-Agent, под которым токен был выписан.
type SearchToken struct {
	Cookie    string // полная cookie-строка из браузера
	UserAgent string // UA, которым получен токен (может быть пустым → дефолт)
}

// Valid — есть ли хоть какой-то токен.
func (t SearchToken) Valid() bool { return t.Cookie != "" }

// TokenProvider отдаёт актуальный токен WB-поиска. Реализации: чтение из Redis
// (прод) либо статическая (тесты/локально).
type TokenProvider interface {
	Token(ctx context.Context) (SearchToken, error)
}

// StaticTokenProvider — фиксированный токен (тесты, локальный запуск).
type StaticTokenProvider struct{ T SearchToken }

func (s StaticTokenProvider) Token(context.Context) (SearchToken, error) { return s.T, nil }

// TokenProviderFunc — адаптер: обычная функция как TokenProvider.
// Удобно обернуть чтение из Redis в main без отдельного типа.
type TokenProviderFunc func(ctx context.Context) (SearchToken, error)

func (f TokenProviderFunc) Token(ctx context.Context) (SearchToken, error) { return f(ctx) }
