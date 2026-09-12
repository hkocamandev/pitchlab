package api

import (
	"net/http"

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

	summary, err := s.store.GetAthleteSummary(r.Context(), dbgen.GetAthleteSummaryParams{
		PitcherAthleteID: id, FromTs: from, ToTs: to,
	})
	if err != nil {
		return httpx.ErrInternal(err)
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
		dist, err := s.store.GetAthletePitchTypeDistribution(r.Context(),
			dbgen.GetAthletePitchTypeDistributionParams{
				PitcherAthleteID: id, FromTs: from, ToTs: to,
			})
		if err != nil {
			return httpx.ErrInternal(err)
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
		trend, err := s.store.GetAthleteSessionTrend(r.Context(),
			dbgen.GetAthleteSessionTrendParams{PitcherAthleteID: id, Limit: 60})
		if err != nil {
			return httpx.ErrInternal(err)
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

	open, err := s.store.CountOpenAnomalies(r.Context(), id)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	out.OpenAnomalyCount = open

	// Analytics is expensive and tolerates being a minute stale, which is
	// also what makes it the right thing to cache in Redis later.
	w.Header().Set("Cache-Control", "private, max-age=60")
	httpx.WriteJSON(w, r, http.StatusOK, out)
	return nil
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
