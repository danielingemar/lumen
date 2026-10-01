// Package store defines the storage interface and the ClickHouse implementation.
package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/danielingemar/lumen/internal/model"
)

// Store is the seam between the API and the database. Every method is tenant-scoped:
// implementations MUST filter by tenant so one customer can never read another's data.
type Store interface {
	InsertSpans(ctx context.Context, rows []model.Span) error
	InsertLogs(ctx context.Context, rows []model.LogRecord) error
	InsertMetrics(ctx context.Context, rows []model.MetricPoint) error

	QueryTraces(ctx context.Context, tenant string, q model.TraceQuery) ([]json.RawMessage, error)
	GetTrace(ctx context.Context, tenant, traceID string) ([]json.RawMessage, error)
	QueryLogs(ctx context.Context, tenant string, q model.LogQuery) ([]json.RawMessage, error)

	// Dashboard data
	Series(ctx context.Context, tenant string, q model.SeriesQuery) ([]model.Series, error)
	Services(ctx context.Context, tenant string, from, to time.Time) ([]json.RawMessage, error)
	MetricNames(ctx context.Context, tenant, service string, from, to time.Time) ([]json.RawMessage, error)
	MetricLabels(ctx context.Context, tenant, name string, from, to time.Time) ([]json.RawMessage, error)

	Latest(ctx context.Context, tenant string, names []string, from time.Time) ([]model.Latest, error)

	Ping(ctx context.Context) error
}
