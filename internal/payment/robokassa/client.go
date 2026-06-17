// Package robokassa — минимальный клиент Робокассы под нужды бота: собрать
// подписанную ссылку на оплату, проверить подпись уведомления (ResultURL) и
// провести автосписание (рекуррент). Чек НПД (самозанятый) прикладываем
// параметром Receipt. Док: https://docs.robokassa.ru
package robokassa

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	payBaseURL       = "https://auth.robokassa.ru/Merchant/Index.aspx"
	recurringBaseURL = "https://auth.robokassa.ru/Merchant/Recurring"
)

// Client — клиент Робокассы. password1 — для подписи инициализации платежа,
// password2 — для проверки подписи в уведомлении (ResultURL).
type Client struct {
	login     string
	password1 string
	password2 string
	isTest    bool
	sno       string // система налогообложения для чека; "" → не передаём (самозанятый)
	hashType  string // md5 | sha256 | sha512 (как настроено в ЛК)

	payURL       string
	recurringURL string
	http         *http.Client
}

// Config — параметры клиента Робокассы.
type Config struct {
	Login     string
	Password1 string
	Password2 string
	IsTest    bool
	SNO       string // "npd"/"" для самозанятого (sno в чек не кладём); иначе osn/usn_income/...
	HashType  string // пусто → md5
}

// NewClient — клиент с дефолтным http.Client (proxy из окружения, таймаут 20с).
func NewClient(cfg Config) *Client {
	ht := strings.ToLower(cfg.HashType)
	if ht == "" {
		ht = "md5"
	}
	// У Робокассы НЕТ кода sno для НПД (только osn/usn_income/usn_income_outcome/
	// esn/patent). Для самозанятого sno в чек не кладём — Робокасса берёт систему
	// налогообложения из ЛК. Поэтому "npd" (и пустое) → "" = sno опускаем; иначе
	// валидное значение прокидываем как есть. Невалидный sno даёт ошибку 29.
	sno := strings.ToLower(strings.TrimSpace(cfg.SNO))
	if sno == "npd" {
		sno = ""
	}
	return &Client{
		login:        cfg.Login,
		password1:    cfg.Password1,
		password2:    cfg.Password2,
		isTest:       cfg.IsTest,
		sno:          sno,
		hashType:     ht,
		payURL:       payBaseURL,
		recurringURL: recurringBaseURL,
		http:         &http.Client{Timeout: 20 * time.Second},
	}
}

// ReceiptItem — позиция чека. Sum совпадает с OutSum (одна услуга на всю сумму).
type ReceiptItem struct {
	Name          string  `json:"name"`
	Quantity      int     `json:"quantity"`
	Sum           float64 `json:"sum"`
	PaymentMethod string  `json:"payment_method"` // full_prepayment
	PaymentObject string  `json:"payment_object"` // service
	Tax           string  `json:"tax"`            // none (самозанятый без НДС)
}

// Receipt — фискальный чек для Робокассы (URL-encoded JSON в параметре Receipt).
type Receipt struct {
	SNO   string        `json:"sno,omitempty"`
	Items []ReceiptItem `json:"items"`
}

// PaymentParams — данные для ссылки на оплату.
type PaymentParams struct {
	InvID       int64  // числовой id заказа (= наш payments.id)
	OutSum      string // сумма "189.00"
	Description string
	Email       string
	Recurring   bool     // первый платёж подписки
	Receipt     *Receipt // nil → без чека
}

// receiptEncoded — компактный JSON чека в URL-encoded виде (в этом виде он идёт
// и в подпись, и в URL — url.Values.Encode применит то же QueryEscape). Если у
// чека не задана система налогообложения, подставляем sno клиента (пустой → sno
// в JSON не попадёт благодаря omitempty: самозанятый sno не передаёт).
func (c *Client) receiptEncoded(r *Receipt) (raw, encoded string, err error) {
	if r == nil {
		return "", "", nil
	}
	if r.SNO == "" {
		r.SNO = c.sno
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", "", err
	}
	return string(b), url.QueryEscape(string(b)), nil
}

// BuildPaymentURL — подписанная ссылка на оплату.
// Подпись: MerchantLogin:OutSum:InvId[:Receipt]:Password1 (Receipt — URL-encoded).
func (c *Client) BuildPaymentURL(p PaymentParams) (string, error) {
	invID := strconv.FormatInt(p.InvID, 10)
	rawReceipt, encReceipt, err := c.receiptEncoded(p.Receipt)
	if err != nil {
		return "", fmt.Errorf("robokassa: marshal receipt: %w", err)
	}

	sigParts := []string{c.login, p.OutSum, invID}
	if encReceipt != "" {
		sigParts = append(sigParts, encReceipt)
	}
	sigParts = append(sigParts, c.password1)
	sig := c.hashHex(strings.Join(sigParts, ":"))

	q := url.Values{}
	q.Set("MerchantLogin", c.login)
	q.Set("OutSum", p.OutSum)
	q.Set("InvId", invID)
	if p.Description != "" {
		q.Set("Description", p.Description)
	}
	if rawReceipt != "" {
		// Сырой JSON: q.Encode() применит QueryEscape — совпадёт с encReceipt в подписи.
		q.Set("Receipt", rawReceipt)
	}
	q.Set("SignatureValue", sig)
	if p.Email != "" {
		q.Set("Email", p.Email)
	}
	if p.Recurring {
		q.Set("Recurring", "true")
	}
	if c.isTest {
		q.Set("IsTest", "1")
	}
	return c.payURL + "?" + q.Encode(), nil
}

// VerifyResult проверяет подпись уведомления ResultURL.
// Подпись: OutSum:InvId:Password2 (без доп. shp-параметров — мы их не шлём).
func (c *Client) VerifyResult(outSum, invID, signature string) bool {
	want := c.hashHex(outSum + ":" + invID + ":" + c.password2)
	return strings.EqualFold(want, signature)
}

// RecurringParams — данные для автосписания по сохранённой связке.
type RecurringParams struct {
	InvID         int64  // новый числовой id (= новый payments.id)
	PreviousInvID int64  // InvId первого платежа подписки
	OutSum        string // сумма "189.00"
	Description   string
	Receipt       *Receipt // nil → без чека
}

// ChargeRecurring проводит автосписание (S2S POST на /Recurring). Подпись как у
// инициализации, но с новым InvId. Результат списания асинхронно придёт на
// ResultURL (как обычная оплата). Возвращает ошибку, если Робокасса не приняла.
func (c *Client) ChargeRecurring(ctx context.Context, p RecurringParams) error {
	invID := strconv.FormatInt(p.InvID, 10)
	rawReceipt, encReceipt, err := c.receiptEncoded(p.Receipt)
	if err != nil {
		return fmt.Errorf("robokassa: marshal receipt: %w", err)
	}

	sigParts := []string{c.login, p.OutSum, invID}
	if encReceipt != "" {
		sigParts = append(sigParts, encReceipt)
	}
	sigParts = append(sigParts, c.password1)
	sig := c.hashHex(strings.Join(sigParts, ":"))

	form := url.Values{}
	form.Set("MerchantLogin", c.login)
	form.Set("InvoiceID", invID)
	form.Set("PreviousInvoiceID", strconv.FormatInt(p.PreviousInvID, 10))
	form.Set("OutSum", p.OutSum)
	if p.Description != "" {
		form.Set("Description", p.Description)
	}
	if rawReceipt != "" {
		form.Set("Receipt", rawReceipt)
	}
	form.Set("SignatureValue", sig)
	if c.isTest {
		form.Set("IsTest", "1")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.recurringURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("robokassa recurring: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	// Успех — ответ начинается с "OK" (далее номер счёта). Иначе — текст ошибки.
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "OK") {
		return fmt.Errorf("robokassa recurring rejected: %s", strings.TrimSpace(string(body)))
	}
	return nil
}

// hashHex считает подпись выбранным алгоритмом и возвращает hex (нижний регистр).
func (c *Client) hashHex(s string) string {
	var h hash.Hash
	switch c.hashType {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		h = md5.New()
	}
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}
