package domain

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Win-back конца триала (docs/TARIFF-FREE-SEARCH-LINK.md, «Промокоды» п.1):
// главный конвертер — три касания вокруг конца триала с персональной скидкой.
//
//	Стадия 1: за WinbackStage1Lead до конца триала — выдаём персональный
//	          discount-код −WinbackDiscountPct% со сроком до конца триала +
//	          WinbackCodeTail («сроки те же»: код привязан к plan_expires_at,
//	          а не к моменту отправки пуша).
//	Стадия 2: триал истёк — «что осталось на Free + скидка ещё действует».
//	Стадия 3: за WinbackStage3Lead до сгорания кода, только если код не погашен
//	          и юзер не купил — последний звонок.
//
// Больше трёх касаний не делаем: четвёртое почти не добавляет конверсии, но
// приучает игнорировать пуши. Ввод другого (например −50%) кода НЕ сжигает
// персональный: discount гасится только оплатой (docs/CHECKOUT-PROMO.md),
// код остаётся действителен до своего expires_at.
const (
	WinbackDiscountPct = 30
	WinbackStage1Lead  = 48 * time.Hour // пуш 1: за 2 дня до конца триала
	WinbackCodeTail    = 48 * time.Hour // код живёт ещё 2 дня после триала
	WinbackStage3Lead  = 12 * time.Hour // пуш 3: за 12 часов до сгорания кода
)

// Окно отправки маркетинговых пушей (МСК): ночью не шлём — «созревшая» стадия
// уходит первым reconcile-тиком после 10 утра. Дедлайны кода от этого не
// плывут (код привязан к plan_expires_at).
const (
	SendWindowStartHour = 10
	SendWindowEndHour   = 22
)

var mskZone = time.FixedZone("MSK", 3*60*60)

// InSendWindow — можно ли сейчас слать маркетинговый пуш (10:00–21:59 МСК).
func InSendWindow(now time.Time) bool {
	h := now.In(mskZone).Hour()
	return h >= SendWindowStartHour && h < SendWindowEndHour
}

// FormatMSK — дедлайн для текстов пушей: «08.07 в 15:04 МСК».
func FormatMSK(t time.Time) string {
	return t.In(mskZone).Format("02.01 в 15:04") + " МСК"
}

// winbackCodeAlphabet — без похожих символов (0/O, 1/I/L), чтобы код легко
// вводился руками в VK/MAX, где нет tap-to-copy.
const winbackCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// GenerateWinbackCode — персональный код вида BACK30-7KF9Q.
func GenerateWinbackCode() (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = winbackCodeAlphabet[int(b)%len(winbackCodeAlphabet)]
	}
	return fmt.Sprintf("BACK%d-%s", WinbackDiscountPct, buf), nil
}
