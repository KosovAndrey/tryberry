package telegram

import "regexp"

// botTokenRe матчит `bot<id>:<секрет>` — так токен выглядит в URL Bot API.
// Транспортные ошибки Go (*url.Error) печатают URL целиком, поэтому любая
// сетевая неудача getUpdates/sendMessage утаскивала боевой токен в логи, а
// оттуда в Loki (инцидент 2026-08-23: `Post "https://api.telegram.org/bot<токен>
// /getUpdates": EOF` в pt_api при флапе плеча xray).
var botTokenRe = regexp.MustCompile(`bot\d+:[A-Za-z0-9_-]+`)

// ScrubToken — текст ошибки без токена бота. Для логов.
func ScrubToken(err error) string {
	if err == nil {
		return ""
	}
	return botTokenRe.ReplaceAllString(err.Error(), "bot<redacted>")
}

// scrubbed прячет токен в тексте ошибки, сохраняя цепочку для errors.Is/As —
// им нужен исходный err, а печатается уже вычищенный текст.
type scrubbed struct{ err error }

func (e scrubbed) Error() string { return ScrubToken(e.err) }
func (e scrubbed) Unwrap() error { return e.err }

// Scrub оборачивает ошибку так, что её текст больше не содержит токен.
func Scrub(err error) error {
	if err == nil {
		return nil
	}
	return scrubbed{err: err}
}
