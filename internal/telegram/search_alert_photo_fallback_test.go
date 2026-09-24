package telegram

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Фото у алерта — приятное дополнение, но не условие доставки. Ночью 24-09
// sendPhoto повис (Telegram сам ходит за картинкой, CDN отвечал медленно),
// фолбэк на текст срабатывал только для перманентных отказов, и сообщение
// ушло в ретраи, а потом было отброшено — подписчик не узнал о снижении цены.

type fakeRT func(*http.Request) (*http.Response, error)

func (f fakeRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func tgResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// notifierWithCalls — нотифаер, у которого фото падает заданной ошибкой, а
// текст уходит успешно. Возвращает список вызванных методов Bot API.
func notifierWithCalls(t *testing.T, photoFail func() (*http.Response, error)) (*Notifier, *[]string) {
	t.Helper()
	var calls []string
	n := NewNotifier("token", "", nil)
	n.client = &http.Client{Transport: fakeRT(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			calls = append(calls, "sendPhoto")
			return photoFail()
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			calls = append(calls, "sendMessage")
			return tgResp(200, `{"ok":true,"result":{"message_id":1}}`), nil
		default:
			calls = append(calls, r.URL.Path)
			return tgResp(200, `{"ok":true}`), nil
		}
	})}
	return n, &calls
}

func testAlert() SearchAlert {
	return SearchAlert{
		ChatID: 1,
		UserID: 1,
		Items: []SearchAlertItem{{
			Name:         "Тестовый товар",
			ImageURL:     "https://basket-52.wbbasket.ru/vol1/part1/1/images/big/1.webp",
			EffectiveRub: 100,
			PrevRub:      200,
		}},
	}
}

func TestSearchAlertFallsBackToTextOnPhotoTimeout(t *testing.T) {
	n, calls := notifierWithCalls(t, func() (*http.Response, error) {
		return nil, context.DeadlineExceeded // тот самый ночной таймаут
	})

	if err := n.SendSearchAlert(context.Background(), testAlert()); err != nil {
		t.Fatalf("алерт не доставлен: %v", err)
	}
	if len(*calls) != 2 || (*calls)[1] != "sendMessage" {
		t.Fatalf("вызовы %v — ждали фото, затем текстовый фолбэк", *calls)
	}
}

func TestSearchAlertFallsBackToTextOnPhotoServerError(t *testing.T) {
	// 5xx от Telegram на фото — тоже не повод молчать.
	n, calls := notifierWithCalls(t, func() (*http.Response, error) {
		return tgResp(502, `{"ok":false,"description":"Bad Gateway"}`), nil
	})

	if err := n.SendSearchAlert(context.Background(), testAlert()); err != nil {
		t.Fatalf("алерт не доставлен: %v", err)
	}
	if len(*calls) != 2 || (*calls)[1] != "sendMessage" {
		t.Fatalf("вызовы %v — ждали фото, затем текстовый фолбэк", *calls)
	}
}

func TestSearchAlertDoesNotRetryWhenRecipientGone(t *testing.T) {
	// Юзер заблокировал бота — слать ему текст бессмысленно.
	n, calls := notifierWithCalls(t, func() (*http.Response, error) {
		return tgResp(403, `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`), nil
	})

	err := n.SendSearchAlert(context.Background(), testAlert())
	if err == nil {
		t.Fatal("ждали ошибку про ушедшего получателя")
	}
	if len(*calls) != 1 {
		t.Fatalf("вызовы %v — текст слать не следовало", *calls)
	}
}
