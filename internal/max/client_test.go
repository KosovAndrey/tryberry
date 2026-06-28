package max

import (
	"testing"

	"github.com/max-messenger/max-bot-api-client-go/schemes"
)

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
