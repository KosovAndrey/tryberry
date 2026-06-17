package robokassa

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// rawQueryParam достаёт значение параметра из query-строки БЕЗ декодирования
// (нужно, чтобы сверить Receipt в URL с тем, что ушло в подпись).
func rawQueryParam(rawURL, key string) string {
	_, q, _ := strings.Cut(rawURL, "?")
	for _, kv := range strings.Split(q, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func newTestClient() *Client {
	return NewClient(Config{Login: "shop", Password1: "pw1", Password2: "pw2"})
}

func TestBuildPaymentURL_SignatureNoReceipt(t *testing.T) {
	c := newTestClient()
	rawURL, err := c.BuildPaymentURL(PaymentParams{InvID: 42, OutSum: "189.00", Description: "Тариф"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	want := md5hex("shop:189.00:42:pw1")
	if got := u.Query().Get("SignatureValue"); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if got := u.Query().Get("InvId"); got != "42" {
		t.Fatalf("InvId = %q", got)
	}
	if got := u.Query().Get("OutSum"); got != "189.00" {
		t.Fatalf("OutSum = %q", got)
	}
}

func TestBuildPaymentURL_SignatureWithReceiptMatchesURL(t *testing.T) {
	c := newTestClient()
	// Самозанятый: sno в чеке НЕ задаём (у Робокассы нет кода НПД).
	rcpt := &Receipt{
		Items: []ReceiptItem{{
			Name: "Подписка Lite", Quantity: 1, Sum: 189.0,
			PaymentMethod: "full_prepayment", PaymentObject: "service", Tax: "none",
		}},
	}
	rawURL, err := c.BuildPaymentURL(PaymentParams{InvID: 7, OutSum: "189.00", Receipt: rcpt})
	if err != nil {
		t.Fatal(err)
	}

	// Receipt в URL (закодированный q.Encode) должен совпадать с тем, что в подписи.
	encInURL := rawQueryParam(rawURL, "Receipt")
	want := md5hex("shop:189.00:7:" + encInURL + ":pw1")

	u, _ := url.Parse(rawURL)
	if got := u.Query().Get("SignatureValue"); got != want {
		t.Fatalf("signature with receipt = %q, want %q (encReceipt=%q)", got, want, encInURL)
	}

	// sno не должен попасть в чек: невалидный sno=npd → ошибка 29 у Робокассы.
	if rcptJSON := u.Query().Get("Receipt"); strings.Contains(rcptJSON, "sno") {
		t.Fatalf("receipt must not contain sno for self-employed, got %q", rcptJSON)
	}
}

// Явное значение sno (не самозанятый) должно прокидываться в чек как есть.
func TestReceipt_ExplicitSNOPassesThrough(t *testing.T) {
	c := NewClient(Config{Login: "shop", Password1: "pw1", Password2: "pw2", SNO: "usn_income"})
	enc, err := c.receiptEncoded(&Receipt{Items: []ReceiptItem{{Name: "x", Quantity: 1, Sum: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enc, "usn_income") {
		t.Fatalf("explicit sno not in receipt: %q", enc)
	}
}

// Регресс ошибки 29: пробел в чеке должен кодироваться как %20 (RFC 3986), а не
// "+" — иначе .NET-бэкенд Робокассы не сойдётся по подписи.
func TestReceipt_SpaceEncodedAsPercent20(t *testing.T) {
	c := newTestClient()
	enc, err := c.receiptEncoded(&Receipt{Items: []ReceiptItem{{
		Name: "Тариф Lite 30 дней", Quantity: 1, Sum: 199, Tax: "none",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "+") {
		t.Fatalf("receipt must not use + for space, got %q", enc)
	}
	if !strings.Contains(enc, "%20") {
		t.Fatalf("expected %%20 for spaces in receipt, got %q", enc)
	}
}

func TestBuildPaymentURL_RecurringAndTest(t *testing.T) {
	c := NewClient(Config{Login: "shop", Password1: "pw1", Password2: "pw2", IsTest: true})
	rawURL, err := c.BuildPaymentURL(PaymentParams{InvID: 1, OutSum: "1.00", Recurring: true})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(rawURL)
	if u.Query().Get("Recurring") != "true" {
		t.Error("Recurring flag missing")
	}
	if u.Query().Get("IsTest") != "1" {
		t.Error("IsTest flag missing")
	}
}

func TestVerifyResult(t *testing.T) {
	c := newTestClient()
	sig := md5hex("189.00:42:pw2")
	if !c.VerifyResult("189.00", "42", sig) {
		t.Error("valid signature rejected")
	}
	if !c.VerifyResult("189.00", "42", strings.ToUpper(sig)) {
		t.Error("uppercase signature should match (case-insensitive)")
	}
	if c.VerifyResult("189.00", "42", "deadbeef") {
		t.Error("bad signature accepted")
	}
	if c.VerifyResult("190.00", "42", sig) {
		t.Error("tampered OutSum accepted")
	}
}

func TestHashHex_Algorithms(t *testing.T) {
	if got := newTestClient().hashHex("a"); got != md5hex("a") {
		t.Fatalf("md5 default mismatch: %q", got)
	}
	c := NewClient(Config{Login: "s", Password1: "p", HashType: "SHA256"})
	if len(c.hashHex("a")) != 64 {
		t.Fatalf("sha256 hex length = %d, want 64", len(c.hashHex("a")))
	}
}
