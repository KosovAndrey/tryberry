package metrics

import "time"

func WithDBTiming(operation string, fn func() error) error {
	start := time.Now()
	err := fn()
	duration := time.Since(start).Seconds()

	DBQueryDuration.WithLabelValues(operation).Observe(duration)
	status := "success"
	if err != nil {
		status = "error"
	}
	DBQueries.WithLabelValues(operation, status).Inc()

	return err
}
