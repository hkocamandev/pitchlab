package observability

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// QueryTracer times every database query.
//
// Implemented as a pgx tracer rather than by wrapping each call site, because
// the call sites are generated code: adding timing to them would mean either
// editing generated files or writing a hand-maintained wrapper per query, and
// both drift the moment a query is added.
type QueryTracer struct {
	metrics *Metrics
}

// NewQueryTracer builds a tracer.
func NewQueryTracer(m *Metrics) *QueryTracer { return &QueryTracer{metrics: m} }

type queryStartKey struct{}

type queryStart struct {
	name    string
	started time.Time
}

// TraceQueryStart records when the query began.
func (t *QueryTracer) TraceQueryStart(
	ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData,
) context.Context {
	return context.WithValue(ctx, queryStartKey{}, queryStart{
		name:    QueryName(data.SQL),
		started: time.Now(),
	})
}

// TraceQueryEnd records the duration.
func (t *QueryTracer) TraceQueryEnd(
	ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData,
) {
	start, ok := ctx.Value(queryStartKey{}).(queryStart)
	if !ok {
		return
	}
	t.metrics.DBQueryDuration.WithLabelValues(start.name).
		Observe(time.Since(start.started).Seconds())
}

// QueryName extracts a low-cardinality name from a generated query.
//
// The generated SQL keeps its own header comment -- "-- name: GetAthlete :one"
// -- so the name is already in the text and does not have to be threaded
// through the call. That matters for cardinality: the SQL statement itself
// would be a distinct label value per query text, including any that are built
// dynamically, while the name is bounded by the number of queries in the
// repository.
func QueryName(sql string) string {
	const marker = "-- name: "

	idx := strings.Index(sql, marker)
	if idx < 0 {
		// A query with no header is hand-written or built at runtime. It is
		// counted under one bucket rather than under its own text.
		return "unnamed"
	}

	rest := sql[idx+len(marker):]
	if end := strings.IndexAny(rest, " \n\r\t"); end >= 0 {
		rest = rest[:end]
	}
	if rest == "" {
		return "unnamed"
	}
	return rest
}
