// Package config — общие хелперы чтения конфигурации из окружения.
package config

import (
	"fmt"
	"os"
)

// MustEnv возвращает значение обязательной переменной окружения или паникует, если
// она пуста. Раньше эта функция копипастилась в каждом cmd/* — единый источник.
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("env %s is required", key))
	}
	return v
}
