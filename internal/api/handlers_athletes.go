package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/cache"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
	"github.com/hkocamandev/pitchlab/internal/httpx"
)

func (s *Server) listAthletes(w http.ResponseWriter, r *http.Request) error {
	limit, err := httpx.Limit(r, s.cfg.DefaultPageSize, s.cfg.MaxPageSize)
	if err != nil {
		return err
	}
	role, err := httpx.QueryEnum(r, "role", "PITCHER", "BATTER", "TWO_WAY", "UNKNOWN")
	if err != nil {
		return err
	}
	throws, err := httpx.QueryEnum(r, "throws", "L", "R")
	if err != nil {
		return err
	}
	cursor, err := httpx.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}

	params := dbgen.ListAthletesParams{
		// One extra row tells us whether another page exists without paying
		// for a separate COUNT over the whole table.
		Limit:  int32(limit + 1),
		Search: httpx.QueryString(r, "q"),
		Role:   role,
		Throws: throws,
	}
	if cursor.Name != "" && cursor.ID != nil {
		params.CursorName = &cursor.Name
		params.CursorID = cursor.ID
	}

	rows, err := s.store.ListAthletes(r.Context(), params)
	if err != nil {
		return httpx.ErrInternal(err)
	}

	rows, hasMore := httpx.Paginate(rows, limit)
	items := make([]AthleteListItemDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, AthleteListItemDTO{
			AthleteDTO: toAthleteDTO(dbgen.Athlete{
				ID: row.ID, MlbamID: row.MlbamID, FullName: row.FullName,
				Throws: row.Throws, Bats: row.Bats,
				PrimaryRole: row.PrimaryRole, BirthDate: row.BirthDate,
			}),
			SessionCount: row.SessionCount,
			PitchCount:   row.PitchCount,
		})
	}

	page := httpx.Page{Limit: limit, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = httpx.Cursor{Name: last.FullName, ID: &last.ID}.Encode()
	}

	httpx.WriteJSON(w, r, http.StatusOK, httpx.List[AthleteListItemDTO]{
		Data: items, Page: page,
	})
	return nil
}

func (s *Server) getAthlete(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "athleteId")
	if err != nil {
		return err
	}

	athlete, err := s.store.GetAthlete(r.Context(), id)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("athlete")
		}
		return httpx.ErrInternal(err)
	}

	totals, err := s.store.GetAthleteTotals(r.Context(), id)
	if err != nil {
		return httpx.ErrInternal(err)
	}

	httpx.WriteJSON(w, r, http.StatusOK, AthleteDetailDTO{
		AthleteDTO: toAthleteDTO(athlete),
		Totals: AthleteTotalsDTO{
			SessionCount: totals.SessionCount,
			PitchCount:   totals.PitchCount,
			FirstPitchAt: asTimePtr(totals.FirstPitchAt),
			LastPitchAt:  asTimePtr(totals.LastPitchAt),
		},
	})
	return nil
}

func (s *Server) getAthleteAnalytics(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "athleteId")
	if err != nil {
		return err
	}
	from, err := httpx.QueryTime(r, "from")
	if err != nil {
		return err
	}
	to, err := httpx.QueryTime(r, "to")
	if err != nil {
		return err
	}
	if from != nil && to != nil && to.Before(*from) {
		return httpx.ErrValidation("to must not precede from",
			httpx.FieldError{Field: "to", Message: "before from"})
	}

	// Existence is checked first so a bad id returns 404 rather than an empty
	// but successful analytics payload.
	if _, err := s.store.GetAthlete(r.Context(), id); err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("athlete")
		}
		return httpx.ErrInternal(err)
	}

	include := httpx.QueryIncludes(r, "distribution", "trend")

	// Cache-aside. This endpoint runs up to four aggregates over every pitch an
	// athlete has ever thrown, and the page is opened far more often than the
	// numbers change -- which is the only kind of work worth caching.
	//
	// The generation is read once and reused for the store. Reading it again
	// before writing would let an invalidation that lands in between file the
	// fresh payload under the stale generation, where nothing would look for it.
	generation := s.cache.Generation(r.Context(), id)
	variant := analyticsVariant(from, to, include)

	var out AnalyticsDTO
	if s.cache.GetAnalytics(r.Context(), id, generation, variant, &out) {
		writeAnalytics(w, r, out, "HIT")
		return nil
	}

	out, err = s.computeAthleteAnalytics(r.Context(), id, from, to, include)
	if err != nil {
		return err
	}
	s.cache.SetAnalytics(r.Context(), id, generation, variant, out)

	writeAnalytics(w, r, out, "MISS")
	return nil
}

// analyticsVariant identifies which shape of the rollup a request asked for.
//
// The window and the requested sections change the payload, so one athlete has
// several valid cached entries at once. Leaving them out of the key would
// serve a year's numbers to someone who asked for a week.
func analyticsVariant(from, to *time.Time, include map[string]bool) string {
	parts := []string{"from=" + formatTimeParam(from), "to=" + formatTimeParam(to)}
	// Sorted, so two requests that differ only in the order of include values
	// share one entry instead of computing the same thing twice.
	keys := make([]string, 0, len(include))
	for k, v := range include {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts = append(parts, "include="+strings.Join(keys, ","))
	return cache.Variant(parts...)
}

func formatTimeParam(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func writeAnalytics(w http.ResponseWriter, r *http.Request, out AnalyticsDTO, state string) {
	// Reported so a cache problem is visible from the outside rather than only
	// in a metric nobody is looking at.
	w.Header().Set(httpx.HeaderCacheState, state)
	// Analytics is expensive and tolerates being a minute stale, which is also
	// what makes it the right thing to cache.
	w.Header().Set("Cache-Control", "private, max-age=60")
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// computeAthleteAnalytics builds the rollup from PostgreSQL.
func (s *Server) computeAthleteAnalytics(
	ctx context.Context, id uuid.UUID, from, to *time.Time, include map[string]bool,
) (AnalyticsDTO, error) {
	summary, err := s.store.GetAthleteSummary(ctx, dbgen.GetAthleteSummaryParams{
		PitcherAthleteID: id, FromTs: from, ToTs: to,
	})
	if err != nil {
		return AnalyticsDTO{}, httpx.ErrInternal(err)
	}

	n := summary.PitchCount
	out := AnalyticsDTO{
		AthleteID: id,
		Period:    ModelPeriodDTO{Start: from, End: to},
		Summary: AthleteSummaryDTO{
			PitchCount:   n,
			SessionCount: summary.SessionCount,
			// Zero is what SQL returns for an empty group; reporting it as a
			// measurement would be wrong, so it is suppressed instead.
			AvgReleaseSpeed:     nilIfEmpty(summary.AvgReleaseSpeed, n),
			AvgReleaseSpinRate:  nilIfEmpty(summary.AvgReleaseSpinRate, n),
			AvgReleaseExtension: nilIfEmpty(summary.AvgReleaseExtension, n),
			AvgPitchScore:       nilIfEmpty(summary.AvgPitchScore, n),
			WhiffRate:           nilIfEmpty(summary.WhiffRate, n),
			CalledStrikeRate:    nilIfEmpty(summary.CalledStrikeRate, n),
			InZoneRate:          nilIfEmpty(summary.InZoneRate, n),
		},
	}

	if include["distribution"] {
		dist, err := s.store.GetAthletePitchTypeDistribution(ctx,
			dbgen.GetAthletePitchTypeDistributionParams{
				PitcherAthleteID: id, FromTs: from, ToTs: to,
			})
		if err != nil {
			return AnalyticsDTO{}, httpx.ErrInternal(err)
		}
		var total int64
		for _, d := range dist {
			total += d.N
		}
		for _, d := range dist {
			stat := PitchTypeStatDTO{
				Count:              d.N,
				PitchName:          asStringPtr(d.PitchName),
				AvgReleaseSpeed:    nilIfEmpty(d.AvgReleaseSpeed, d.N),
				AvgReleaseSpinRate: nilIfEmpty(d.AvgReleaseSpinRate, d.N),
				AvgPfxXArm:         nilIfEmpty(d.AvgPfxXArm, d.N),
				AvgPfxZ:            nilIfEmpty(d.AvgPfxZ, d.N),
				AvgPitchScore:      nilIfEmpty(d.AvgPitchScore, d.N),
				WhiffRate:          nilIfEmpty(d.WhiffRate, d.N),
			}
			if d.PitchType != nil {
				stat.PitchType = *d.PitchType
			}
			if total > 0 {
				stat.UsagePct = float64(d.N) / float64(total)
			}
			out.PitchTypeDistribution = append(out.PitchTypeDistribution, stat)
		}
	}

	if include["trend"] {
		trend, err := s.store.GetAthleteSessionTrend(ctx,
			dbgen.GetAthleteSessionTrendParams{PitcherAthleteID: id, Limit: 60})
		if err != nil {
			return AnalyticsDTO{}, httpx.ErrInternal(err)
		}
		for _, t := range trend {
			out.Trend = append(out.Trend, TrendPointDTO{
				SessionID: t.SessionID, SourceGameDate: t.SourceGameDate,
				StartedAt: t.StartedAt, PitchCount: t.PitchCount,
				AvgReleaseSpeed:    t.AvgReleaseSpeed,
				AvgReleaseSpinRate: t.AvgReleaseSpinRate,
				AvgPitchScore:      t.AvgPitchScore,
			})
		}
	}

	open, err := s.store.CountOpenAnomalies(ctx, id)
	if err != nil {
		return AnalyticsDTO{}, httpx.ErrInternal(err)
	}
	out.OpenAnomalyCount = open

	return out, nil
}

func (s *Server) listAthleteSessions(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "athleteId")
	if err != nil {
		return err
	}
	limit, err := httpx.Limit(r, s.cfg.DefaultPageSize, s.cfg.MaxPageSize)
	if err != nil {
		return err
	}
	status, err := httpx.QueryEnum(r, "status", "ACTIVE", "COMPLETED", "ABORTED")
	if err != nil {
		return err
	}
	cursor, err := httpx.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}

	if _, err := s.store.GetAthlete(r.Context(), id); err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("athlete")
		}
		return httpx.ErrInternal(err)
	}

	params := dbgen.ListSessionsByAthleteParams{
		PitcherAthleteID: id, Limit: int32(limit + 1), Status: status,
	}
	if cursor.Time != nil && cursor.ID != nil {
		params.CursorStarted = cursor.Time
		params.CursorID = cursor.ID
	}

	rows, err := s.store.ListSessionsByAthlete(r.Context(), params)
	if err != nil {
		return httpx.ErrInternal(err)
	}

	rows, hasMore := httpx.Paginate(rows, limit)
	items := make([]SessionDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toSessionDTO(row))
	}

	page := httpx.Page{Limit: limit, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = httpx.Cursor{Time: &last.StartedAt, ID: &last.ID}.Encode()
	}

	httpx.WriteJSON(w, r, http.StatusOK, httpx.List[SessionDTO]{Data: items, Page: page})
	return nil
}

func (s *Server) listAthleteAnomalies(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "athleteId")
	if err != nil {
		return err
	}
	limit, err := httpx.Limit(r, s.cfg.DefaultPageSize, s.cfg.MaxPageSize)
	if err != nil {
		return err
	}
	severity, err := httpx.QueryEnum(r, "severity", "MEDIUM", "HIGH")
	if err != nil {
		return err
	}
	acknowledged, err := httpx.QueryBool(r, "acknowledged")
	if err != nil {
		return err
	}
	cursor, err := httpx.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}

	if _, err := s.store.GetAthlete(r.Context(), id); err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("athlete")
		}
		return httpx.ErrInternal(err)
	}

	params := dbgen.ListAnomaliesByAthleteParams{
		AthleteID: id, Limit: int32(limit + 1),
		Severity: severity, Metric: httpx.QueryString(r, "metric"),
		Acknowledged: acknowledged,
	}
	if cursor.Time != nil && cursor.ID != nil {
		params.CursorDetected = cursor.Time
		params.CursorID = cursor.ID
	}

	rows, err := s.store.ListAnomaliesByAthlete(r.Context(), params)
	if err != nil {
		return httpx.ErrInternal(err)
	}

	rows, hasMore := httpx.Paginate(rows, limit)
	items := make([]AnomalyDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toAnomalyDTO(row))
	}

	page := httpx.Page{Limit: limit, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = httpx.Cursor{Time: &last.DetectedAt, ID: &last.ID}.Encode()
	}

	httpx.WriteJSON(w, r, http.StatusOK, httpx.List[AnomalyDTO]{Data: items, Page: page})
	return nil
}

// acknowledgeAnomaly marks an alert as reviewed.
//
// The only write a user makes. Without it the alert list becomes unusable as
// history accumulates, which is why performance_anomalies has an
// acknowledged_at column at all.
func (s *Server) acknowledgeAnomaly(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "anomalyId")
	if err != nil {
		return err
	}

	updated, err := s.store.AcknowledgeAnomaly(r.Context(), id)
	if err == nil {
		httpx.WriteJSON(w, r, http.StatusOK, toAnomalyDTO(updated))
		return nil
	}
	if !db.IsNoRows(err) {
		return httpx.ErrInternal(err)
	}

	// The guarded UPDATE matched nothing. That is either "does not exist" or
	// "already acknowledged", and the caller deserves to know which.
	if _, getErr := s.store.GetAnomaly(r.Context(), id); getErr != nil {
		if db.IsNoRows(getErr) {
			return httpx.ErrNotFound("anomaly")
		}
		return httpx.ErrInternal(getErr)
	}
	return httpx.ErrConflict("anomaly is already acknowledged")
}
