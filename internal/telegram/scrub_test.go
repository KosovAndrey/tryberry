package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

const fakeToken = "8601948194:AAGJOkqh6ixS_2caz_44sTFNEc7-mWUXPrg"

func TestScrubTokenHidesTokenFromTransportError(t *testing.T) {
	err := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + fakeToken + "/getUpdates",
		Err: errors.New("EOF"),
	}

	got := ScrubToken(err)
	if strings.Contains(got, fakeToken) {
		t.Fatalf("токен остался в тексте ошибки: %s", got)
	}
	if !strings.Contains(got, "bot<redacted>") || !strings.Contains(got, "getUpdates") {
		t.Fatalf("вычищено слишком много, диагностика потеряна: %s", got)
	}
}

func TestScrubKeepsErrorChain(t *testing.T) {
	wrapped := fmt.Errorf("http do: %w", Scrub(fmt.Errorf(
		`Post "https://api.telegram.org/bot%s/sendMessage": %w`, fakeToken, context.Canceled)))

	if !errors.Is(wrapped, context.Canceled) {
		t.Fatal("errors.Is перестал видеть исходную причину")
	}
	if strings.Contains(wrapped.Error(), fakeToken) {
		t.Fatalf("токен остался в тексте ошибки: %s", wrapped.Error())
	}
}

func TestScrubTokenNil(t *testing.T) {
	if ScrubToken(nil) != "" || Scrub(nil) != nil {
		t.Fatal("nil должен оставаться nil")
	}
}
