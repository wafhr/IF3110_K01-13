package telemetry

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"
)

const metricNamespace = "if3110"

var durationBuckets = []float64{.005, .01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

type Metrics struct {
	registry      *prometheus.Registry
	queueWait     prometheus.Histogram
	lockWait      prometheus.Histogram
	migrationTime *prometheus.HistogramVec
	retries       prometheus.Counter
	batches       *prometheus.CounterVec
	rows          *prometheus.CounterVec
	pools         *poolCollector
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry:  prometheus.NewRegistry(),
		queueWait: prometheus.NewHistogram(durationMetric("migration_queue_wait_seconds", "Waktu dari job siap diambil hingga di-claim worker.")),
		lockWait:  prometheus.NewHistogram(durationMetric("migration_lock_wait_seconds", "Waktu sampai worker memperoleh lock target.")),
		migrationTime: prometheus.NewHistogramVec(durationMetric("migration_duration_seconds",
			"Durasi satu attempt worker, dari awal attempt hingga hasil eksekusi diketahui."), []string{"action", "outcome"}),
		retries: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "migration_retries_total", Help: "Jumlah retry yang berhasil dijadwalkan kembali.",
		}),
		batches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "migration_batches_total", Help: "Jumlah batch yang berhasil di-commit oleh proses ini.",
		}, []string{"operation"}),
		rows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "migration_rows_total", Help: "Jumlah row terdampak oleh batch yang berhasil di-commit oleh proses ini.",
		}, []string{"operation"}),
		pools: newPoolCollector(),
	}
	m.registry.MustRegister(m.queueWait, m.lockWait, m.migrationTime, m.retries, m.batches, m.rows, m.pools)
	for _, operation := range []string{"execute", "revert"} {
		m.batches.WithLabelValues(operation)
		m.rows.WithLabelValues(operation)
	}
	return m
}

func durationMetric(name, help string) prometheus.HistogramOpts {
	return prometheus.HistogramOpts{Namespace: metricNamespace, Name: name, Help: help, Buckets: durationBuckets}
}

type metricsKey struct{}

func (m *Metrics) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, metricsKey{}, m)
}

func (m *Metrics) Attach(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(m.Context(r.Context())))
	})
}

func MetricsFromContext(ctx context.Context) *Metrics {
	m, _ := ctx.Value(metricsKey{}).(*Metrics)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// Serve membuka endpoint scrape pada listener tersendiri jika alamat diisi.
func (m *Metrics) Serve(address string, shutdownTimeout time.Duration) (func() error, error) {
	if address == "" {
		return func() error { return nil }, nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, errors.New("open metrics listener")
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics_server_failed", "status", "failed")
		}
	}()
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := server.Shutdown(ctx)
		if err != nil {
			_ = server.Close()
		}
		return err
	}, nil
}

// RegisterJobCounts dipasang di server agar status antrean bersama terekspos sekali.
func (m *Metrics) RegisterJobCounts(read func(context.Context) (map[string]int64, error)) {
	desc := prometheus.NewDesc(metricNamespace+"_migration_jobs", "Jumlah job tersimpan menurut status saat scrape.", []string{"status"}, nil)
	m.registry.MustRegister(jobCountsCollector{desc: desc, read: read})
}

type jobCountsCollector struct {
	desc *prometheus.Desc
	read func(context.Context) (map[string]int64, error)
}

func (c jobCountsCollector) Describe(out chan<- *prometheus.Desc) { out <- c.desc }

func (c jobCountsCollector) Collect(out chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	counts, err := c.read(ctx)
	if err != nil {
		out <- prometheus.NewInvalidMetric(c.desc, errors.New("read migration queue status counts"))
		return
	}
	for status, count := range counts {
		out <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), status)
	}
}

func (m *Metrics) ObserveQueueWait(ctx context.Context, seconds float64) {
	if m != nil {
		observeDuration(ctx, m.queueWait, seconds)
	}
}

func (m *Metrics) ObserveLockWait(ctx context.Context, duration time.Duration) {
	if m != nil {
		observeDuration(ctx, m.lockWait, duration.Seconds())
	}
}

func (m *Metrics) ObserveMigration(ctx context.Context, action string, duration time.Duration, err error) {
	if m == nil {
		return
	}
	switch action {
	case "apply", "rollback", "forward", "compensate":
	default:
		action = "unknown"
	}
	outcome := "success"
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		outcome = "canceled"
	default:
		outcome = "failed"
	}
	observeDuration(ctx, m.migrationTime.WithLabelValues(action, outcome), duration.Seconds())
}

func observeDuration(ctx context.Context, observer prometheus.Observer, seconds float64) {
	span := trace.SpanContextFromContext(ctx)
	if exemplar, ok := observer.(prometheus.ExemplarObserver); ok && span.IsSampled() {
		exemplar.ObserveWithExemplar(seconds, prometheus.Labels{"trace_id": span.TraceID().String()})
		return
	}
	observer.Observe(seconds)
}

func (m *Metrics) RecordRetry() {
	if m != nil {
		m.retries.Inc()
	}
}

func (m *Metrics) RecordBatch(result sql.Result, reverted bool) {
	if m == nil {
		return
	}
	operation := "execute"
	if reverted {
		operation = "revert"
	}
	m.batches.WithLabelValues(operation).Inc()
	if count, err := result.RowsAffected(); err == nil && count > 0 {
		m.rows.WithLabelValues(operation).Add(float64(count))
	}
}

type poolLabels struct{ role, system string }

type poolTotals struct {
	inUse, idle, waits int64
	waitTime           time.Duration
}

type poolCollector struct {
	mu          sync.Mutex
	live        map[*sql.DB]poolLabels
	closed      map[poolLabels]poolTotals
	connections *prometheus.Desc
	waitCount   *prometheus.Desc
	waitTime    *prometheus.Desc
}

func newPoolCollector() *poolCollector {
	return &poolCollector{
		live:        make(map[*sql.DB]poolLabels),
		closed:      make(map[poolLabels]poolTotals),
		connections: prometheus.NewDesc(metricNamespace+"_db_pool_connections", "Jumlah koneksi pool menurut kondisi.", []string{"role", "db_system", "state"}, nil),
		waitCount:   prometheus.NewDesc(metricNamespace+"_db_pool_waits_total", "Jumlah operasi yang menunggu koneksi dari pool.", []string{"role", "db_system"}, nil),
		waitTime:    prometheus.NewDesc(metricNamespace+"_db_pool_wait_seconds_total", "Akumulasi waktu tunggu memperoleh koneksi dari pool.", []string{"role", "db_system"}, nil),
	}
}

func (c *poolCollector) Describe(out chan<- *prometheus.Desc) {
	out <- c.connections
	out <- c.waitCount
	out <- c.waitTime
}

func (c *poolCollector) Collect(out chan<- prometheus.Metric) {
	c.mu.Lock()
	totals := make(map[poolLabels]poolTotals, len(c.closed))
	for labels, stats := range c.closed {
		totals[labels] = stats
	}
	for db, labels := range c.live {
		stats, total := db.Stats(), totals[labels]
		total.inUse += int64(stats.InUse)
		total.idle += int64(stats.Idle)
		total.waits += stats.WaitCount
		total.waitTime += stats.WaitDuration
		totals[labels] = total
	}
	c.mu.Unlock()
	for labels, total := range totals {
		out <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(total.inUse), labels.role, labels.system, "in_use")
		out <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(total.idle), labels.role, labels.system, "idle")
		out <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(total.waits), labels.role, labels.system)
		out <- prometheus.MustNewConstMetric(c.waitTime, prometheus.CounterValue, total.waitTime.Seconds(), labels.role, labels.system)
	}
}

type trackedConnector struct {
	driver.Connector
	onClose func()
}

func (c *trackedConnector) Close() error {
	c.onClose()
	if closer, ok := c.Connector.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// OpenSQLPool memakai instrumentasi SQL yang sama dan menautkan metrik ke
// penutupan sql.DB, termasuk pool singkat yang dimiliki setiap attempt.
func OpenSQLPool(ctx context.Context, connector driver.Connector, system, role, database string) *sql.DB {
	wrapped := SQLConnector(connector, system, role, database)
	m := MetricsFromContext(ctx)
	if m == nil {
		return sql.OpenDB(wrapped)
	}
	pools := m.pools
	tracked := &trackedConnector{Connector: wrapped}
	db := sql.OpenDB(tracked)
	labels := poolLabels{role: role, system: system}
	tracked.onClose = func() {
		pools.mu.Lock()
		defer pools.mu.Unlock()
		stats, total := db.Stats(), pools.closed[labels]
		total.waits += stats.WaitCount
		total.waitTime += stats.WaitDuration
		pools.closed[labels] = total
		delete(pools.live, db)
	}
	pools.mu.Lock()
	pools.live[db] = labels
	pools.mu.Unlock()
	return db
}
