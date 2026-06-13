// Package yookassa — минимальный клиент платёжного API ЮKassa (v3),
// только под нужды бота: создать платёж и перечитать его статус.
// Док: https://yookassa.ru/developers/api
package yookassa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const apiBase = "https://api.yookassa.ru/v3"

// Client — клиент API ЮKassa. Аутентификация — HTTP Basic (shopID:secretKey).
// HTTP-клиент по умолчанию уважает HTTPS_PROXY из окружения (RU-egress).
type Client struct {
	shopID    string
	secretKey string
	baseURL   string
	http      *http.Client
}

// NewClient — клиент с дефолтным http.Client (proxy из окружения, таймаут 20с).
func NewClient(shopID, secretKey string) *Client {
	return &Client{
		shopID:    shopID,
		secretKey: secretKey,
		baseURL:   apiBase,
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

// Amount — сумма платежа. Value — строка с двумя знаками после точки ("199.00").
type Amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// Confirmation — способ подтверждения. Для бота — redirect на платёжную форму.
type Confirmation struct {
	Type            string `json:"type"`
	ReturnURL       string `json:"return_url,omitempty"`
	ConfirmationURL string `json:"confirmation_url,omitempty"`
}

// CreateRequest — тело POST /payments. Receipt опционален (см. чеки 54-ФЗ:
// при «Чеках от ЮKassa» в ЛК передавать не нужно).
type CreateRequest struct {
	Amount       Amount            `json:"amount"`
	Capture      bool              `json:"capture"`
	Confirmation Confirmation      `json:"confirmation"`
	Description  string            `json:"description,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Receipt      *Receipt          `json:"receipt,omitempty"`
}

// Receipt — фискальный чек (54-ФЗ), путь «передаём receipt сами».
type Receipt struct {
	Customer ReceiptCustomer `json:"customer"`
	Items    []ReceiptItem   `json:"items"`
}

type ReceiptCustomer struct {
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

type ReceiptItem struct {
	Description string `json:"description"`
	Quantity    string `json:"quantity"`
	Amount      Amount `json:"amount"`
	VATCode     int    `json:"vat_code"`
	// PaymentSubject/PaymentMode — для услуг подписки полная предоплата.
	PaymentSubject string `json:"payment_subject,omitempty"`
	PaymentMode    string `json:"payment_mode,omitempty"`
}

// Payment — ответ ЮKassa по платежу.
type Payment struct {
	ID           string            `json:"id"`
	Status       string            `json:"status"` // pending|waiting_for_capture|succeeded|canceled
	Paid         bool              `json:"paid"`
	Amount       Amount            `json:"amount"`
	Confirmation Confirmation      `json:"confirmation"`
	Metadata     map[string]string `json:"metadata"`
}

// Статусы платежа ЮKassa.
const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusCanceled  = "canceled"
)

// CreatePayment создаёт платёж. idempotenceKey защищает от дублей при ретраях
// (повтор с тем же ключом вернёт тот же платёж, а не создаст новый).
func (c *Client) CreatePayment(ctx context.Context, idempotenceKey string, req CreateRequest) (*Payment, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/payments", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotence-Key", idempotenceKey)
	httpReq.SetBasicAuth(c.shopID, c.secretKey)
	return c.do(httpReq)
}

// GetPayment перечитывает платёж по id (вебхуки ЮKassa не подписаны — статус
// берём из авторитетного источника, а не из тела вебхука).
func (c *Client) GetPayment(ctx context.Context, id string) (*Payment, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/payments/"+id, nil)
	if err != nil {
		return nil, err
	}
	httpReq.SetBasicAuth(c.shopID, c.secretKey)
	return c.do(httpReq)
}

func (c *Client) do(req *http.Request) (*Payment, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("yookassa: %s: %s", resp.Status, string(data))
	}
	var p Payment
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("yookassa: decode response: %w", err)
	}
	return &p, nil
}
