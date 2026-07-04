// cmd/seed-loadtest — нагрузочный тест прода синтетическими юзерами
// (docs/LOAD-TEST-SYNTHETIC.md). Три подкоманды:
//
//	collect — обойти поисковую выдачу маркетплейсов по списку популярных
//	          запросов и собрать реальные товары в JSONL (URL/имя/цена).
//	seed    — создать N синтетических юзеров (is_synthetic=true) с раздачей
//	          товаров по Zipf; конвейер scheduler→scraper→notifier дальше
//	          нагружает себя сам по штатным кадансам тарифов.
//	cleanup — удалить синтетических юзеров и их подписки; товары и
//	          price_history остаются (накопленные данные для графиков).
//
// Запуск на проде — one-off контейнером из образа сервиса (см. док).
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "collect":
		err = runCollect(log, os.Args[2:])
	case "seed":
		err = runSeed(log, os.Args[2:])
	case "cleanup":
		err = runCleanup(log, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error(os.Args[1]+" failed", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: seed-loadtest <collect|seed|cleanup> [flags]

collect  -queries <file> -out products.jsonl [-mp wildberries,yandex_market,aliexpress,ozon] [-per-query 60]
seed     -in products.jsonl [-users 1000] [-zipf 1.0] [-rand-seed 42] -yes
cleanup  -yes`)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
