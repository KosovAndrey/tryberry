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

func TestBuildPaymentURL_SignatureWithReceiptUsesRawJSON(t *testing.T) {
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

	u, _ := url.Parse(rawURL)
	// Робокасса подписывает СЫРОЙ (декодированный) JSON чека — url.Values.Get
	// возвращает именно декодированное значение параметра Receipt.
	rawReceipt := u.Query().Get("Receipt")
	if !strings.HasPrefix(rawReceipt, `{"items":`) {
		t.Fatalf("Receipt в URL должен декодироваться в сырой JSON, got %q", rawReceipt)
	}
	want := md5hex("shop:189.00:7:" + rawReceipt + ":pw1")
	if got := u.Query().Get("SignatureValue"); got != want {
		t.Fatalf("signature with receipt = %q, want %q (rawReceipt=%q)", got, want, rawReceipt)
	}

	// sno не должен попасть в чек: невалидный sno=npd → ошибка 29 у Робокассы.
	if strings.Contains(rawReceipt, "sno") {
		t.Fatalf("receipt must not contain sno for self-employed, got %q", rawReceipt)
	}
}

// Явное значение sno (не самозанятый) должно прокидываться в чек как есть.
func TestReceipt_ExplicitSNOPassesThrough(t *testing.T) {
	c := NewClient(Config{Login: "shop", Password1: "pw1", Password2: "pw2", SNO: "usn_income"})
	raw, err := c.receiptJSON(&Receipt{Items: []ReceiptItem{{Name: "x", Quantity: 1, Sum: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"sno":"usn_income"`) {
		t.Fatalf("explicit sno not in receipt: %q", raw)
	}
}

// Регресс ошибки 29: в подпись идёт СЫРОЙ JSON, без URL-кодирования (пробелы и
// двоеточия остаются как есть, а не %20/%3A).
func TestReceipt_SignedAsRawJSON(t *testing.T) {
	c := newTestClient()
	raw, err := c.receiptJSON(&Receipt{Items: []ReceiptItem{{
		Name: "Тариф Lite 30 дней", Quantity: 1, Sum: 199, Tax: "none",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(raw, "%+") {
		t.Fatalf("raw receipt JSON must not be URL-encoded, got %q", raw)
	}
	if !strings.Contains(raw, "Тариф Lite 30 дней") {
		t.Fatalf("expected literal name with spaces, got %q", raw)
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
