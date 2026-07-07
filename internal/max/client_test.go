package max

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/schemes"
)

// roundTripFunc — стаб транспорта: ответы без реальной сети (httptest требует
// loopback-TCP, которого в песочнице нет).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTestClient — Client без живого MAX API с подставным транспортом для
// скачивания картинки (api не дёргаем).
func newTestClient(rt roundTripFunc) *Client {
	return &Client{
		log:        slog.Default(),
		imgHTTP:    &http.Client{Transport: rt},
		photoCache: make(map[string]photoCacheEntry),
	}
}

func rawResp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestDownloadImage(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB}, 1024)
	c := newTestClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/ok":
			return rawResp(200, body), nil
		case "/big":
			return rawResp(200, bytes.Repeat([]byte{0x01}, maxImageBytes+10)), nil
		case "/empty":
			return rawResp(200, nil), nil
		case "/neterr":
			return nil, fmt.Errorf("boom")
		default:
			return rawResp(500, nil), nil
		}
	})
	ctx := context.Background()

	t.Run("ok", func(t *testing.T) {
		data, err := c.downloadImage(ctx, "http://mp/ok")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(data, body) {
			t.Fatalf("body mismatch: got %d bytes", len(data))
		}
	})
	t.Run("non-200 → error", func(t *testing.T) {
		if _, err := c.downloadImage(ctx, "http://mp/500"); err == nil {
			t.Fatal("want error on non-200")
		}
	})
	t.Run("too large → error", func(t *testing.T) {
		if _, err := c.downloadImage(ctx, "http://mp/big"); err == nil {
			t.Fatal("want error on oversize image")
		}
	})
	t.Run("empty body → error", func(t *testing.T) {
		if _, err := c.downloadImage(ctx, "http://mp/empty"); err == nil {
			t.Fatal("want error on empty body")
		}
	})
	t.Run("transport error → error", func(t *testing.T) {
		if _, err := c.downloadImage(ctx, "http://mp/neterr"); err == nil {
			t.Fatal("want error on transport failure")
		}
	})
}

func TestPhotoCache(t *testing.T) {
	c := newTestClient(nil)
	tokens := &schemes.PhotoTokens{Photos: map[string]schemes.PhotoToken{"x": {Token: "tok"}}}

	if got := c.cachedPhoto("u"); got != nil {
		t.Fatalf("miss expected, got %+v", got)
	}
	c.cachePhoto("u", tokens)
	if got := c.cachedPhoto("u"); got != tokens {
		t.Fatalf("cache hit expected same pointer, got %+v", got)
	}

	// Протухший вход — промах.
	c.mu.Lock()
	c.photoCache["stale"] = photoCacheEntry{tokens: tokens, exp: time.Now().Add(-time.Minute)}
	c.mu.Unlock()
	if got := c.cachedPhoto("stale"); got != nil {
		t.Fatalf("expired entry must be a miss, got %+v", got)
	}
}

// TestResolvePhotoTokensCacheHit — при попадании в кэш сеть не трогаем (скачивание
// и upload не вызываются).
func TestResolvePhotoTokensCacheHit(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return rawResp(500, nil), nil
	})
	tokens := &schemes.PhotoTokens{Photos: map[string]schemes.PhotoToken{"x": {Token: "tok"}}}
	c.cachePhoto("http://mp/img", tokens)

	got, err := c.resolvePhotoTokens(context.Background(), "http://mp/img")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != tokens {
		t.Fatalf("want cached tokens, got %+v", got)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("cache hit must not download, got %d hits", n)
	}
}

// TestResolvePhotoTokensDownloadError — ошибка скачивания возвращается наверх
// (upload не пробуем), что и уводит SendMessagePhoto в текстовый фолбэк.
func TestResolvePhotoTokensDownloadError(t *testing.T) {
	c := newTestClient(func(r *http.Request) (*http.Response, error) {
		return rawResp(500, nil), nil
	})
	if _, err := c.resolvePhotoTokens(context.Background(), "http://mp/img"); err == nil {
		t.Fatal("want error when download fails")
	}
	if got := c.cachedPhoto("http://mp/img"); got != nil {
		t.Fatal("failed download must not populate cache")
	}
}

func TestDecodeUpdate(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    schemes.UpdateType
		wantErr bool
	}{
		{"message_created", `{"update_type":"message_created","message":{"sender":{"user_id":7},"recipient":{"chat_type":"dialog","chat_id":7},"body":{"text":"hi"}}}`, schemes.TypeMessageCreated, false},
		{"message_callback", `{"update_type":"message_callback","callback":{"callback_id":"c1","payload":"{\"cmd\":\"list\"}","user":{"user_id":7}}}`, schemes.TypeMessageCallback, false},
		{"bot_started", `{"update_type":"bot_started","user":{"user_id":7},"chat_id":7,"payload":"link_ABC"}`, schemes.TypeBotStarted, false},
		{"unsupported", `{"update_type":"message_removed"}`, "", true},
		{"garbage", `{`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upd, err := DecodeUpdate([]byte(c.raw))
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if upd.GetUpdateType() != c.want {
				t.Fatalf("got type %s, want %s", upd.GetUpdateType(), c.want)
			}
		})
	}
}

func TestUserIDFromRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{"from message sender", `{"update_type":"message_created","message":{"sender":{"user_id":42}}}`, 42},
		{"from callback user", `{"update_type":"message_callback","callback":{"user":{"user_id":99}}}`, 99},
		{"from bot_started user", `{"update_type":"bot_started","user":{"user_id":7}}`, 7},
		{"none", `{"update_type":"message_removed"}`, 0},
		{"garbage", `not json`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UserIDFromRaw([]byte(c.raw)); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestParsePayload(t *testing.T) {
	p := parsePayload(`{"cmd":"ptrack","id":12,"k":"below"}`)
	if p.Cmd != "ptrack" || p.ID != 12 || p.Kind != "below" {
		t.Fatalf("unexpected payload: %+v", p)
	}
	if got := parsePayload(""); got.Cmd != "" {
		t.Fatalf("empty payload should be zero value, got %+v", got)
	}
	if got := parsePayload("not-json"); got.Cmd != "" {
		t.Fatalf("invalid payload should be zero value, got %+v", got)
	}
}
