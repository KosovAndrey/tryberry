package metrics

import (
	"net/http"
	"strconv"
	"time"
)

// HTTPMiddleware оборачивает http.Handler и записывает метрики:
// длительность, статус-код, путь. Используй так:
//
//	mux.Handle("/webhook", metrics.HTTPMiddleware("webhook")(yourHandler))
func HTTPMiddleware(routeName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rw, r)

			duration := time.Since(start).Seconds()
			status := strconv.Itoa(rw.status)

			HTTPRequests.WithLabelValues(r.Method, routeName, status).Inc()
			HTTPDuration.WithLabelValues(r.Method, routeName).Observe(duration)
		})
	}
}

// responseWriter перехватывает status code чтобы метрика его учитывала
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
