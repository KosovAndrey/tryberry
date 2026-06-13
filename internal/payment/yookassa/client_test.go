package yookassa

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc — стаб транспорта: канонические ответы без реальной сети
// (httptest требует loopback-TCP, которого в песочнице нет).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubClient(rt roundTripFunc) *Client {
	c := NewClient("shop-1", "secret-key")
	c.http = &http.Client{Transport: rt}
	return c
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestCreatePayment(t *testing.T) {
	var gotAuth, gotIdem, gotPath, gotMethod string
	var body map[string]any

	c := stubClient(func(r *http.Request) (*http.Response, error) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotence-Key")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		return jsonResp(200, `{"id":"2c85f0-test","status":"pending","paid":false,
			"amount":{"value":"199.00","currency":"RUB"},
			"confirmation":{"type":"redirect","confirmation_url":"https://yoomoney.ru/pay/abc"}}`), nil
	})

	p, err := c.CreatePayment(context.Background(), "idem-123", CreateRequest{
		Amount:       Amount{Value: "199.00", Currency: "RUB"},
		Capture:      true,
		Confirmation: Confirmation{Type: "redirect", ReturnURL: "https://t.me/bot"},
		Metadata:     map[string]string{"user_id": "42"},
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v3/payments" {
		t.Errorf("path = %s, want /v3/payments", gotPath)
	}
	if gotIdem != "idem-123" {
		t.Errorf("Idempotence-Key = %q, want idem-123", gotIdem)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic ...", gotAuth)
	}
	if got, _ := body["capture"].(bool); !got {
		t.Errorf("capture not sent as true: %v", body["capture"])
	}
	if p.ID != "2c85f0-test" || p.Status != StatusPending {
		t.Errorf("payment = %+v", p)
	}
	if p.Confirmation.ConfirmationURL != "https://yoomoney.ru/pay/abc" {
		t.Errorf("confirmation_url = %q", p.Confirmation.ConfirmationURL)
	}
}

func TestGetPayment(t *testing.T) {
	c := stubClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v3/payments/pay-9" {
			t.Errorf("path = %s", r.URL.Path)
		}
		return jsonResp(200, `{"id":"pay-9","status":"succeeded","paid":true,
			"amount":{"value":"499.00","currency":"RUB"},"metadata":{"user_id":"7","plan":"pro"}}`), nil
	})

	p, err := c.GetPayment(context.Background(), "pay-9")
	if err != nil {
		t.Fatalf("GetPayment: %v", err)
	}
	if p.Status != StatusSucceeded || p.Metadata["user_id"] != "7" || p.Metadata["plan"] != "pro" {
		t.Errorf("payment = %+v", p)
	}
}

func TestGetPaymentError(t *testing.T) {
	c := stubClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"type":"error","code":"invalid_request"}`), nil
	})
	if _, err := c.GetPayment(context.Background(), "x"); err == nil {
		t.Fatal("expected error on non-2xx, got nil")
	}
}
