package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
	"github.com/hkocamandev/pitchlab/internal/httpx"
)

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) error {
	limit, err := httpx.Limit(r, s.cfg.DefaultPageSize, s.cfg.MaxPageSize)
	if err != nil {
		return err
	}
	status, err := httpx.QueryEnum(r, "status", "ACTIVE", "COMPLETED", "ABORTED")
	if err != nil {
		return err
	}

	// The unfiltered listing is intentionally narrow: it exists to answer
	// "what is live right now", which the partial index makes nearly free.
	// A general cross-athlete session search belongs behind an athlete id.
	if status == nil || *status != "ACTIVE" {
		athleteID, err := httpx.QueryUUID(r, "athlete_id")
		if err != nil {
			return httpx.ErrValidation(
				"athlete_id is required unless status=ACTIVE",
				httpx.FieldError{Field: "athlete_id", Message: "required"})
		}

		cursor, err := httpx.DecodeCursor(r.URL.Query().Get("cursor"))
		if err != nil {
			return err
		}
		params := dbgen.ListSessionsByAthleteParams{
			PitcherAthleteID: athleteID, Limit: int32(limit + 1), Status: status,
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

	rows, err := s.store.ListActiveSessions(r.Context(), int32(limit+1))
	if err != nil {
		return httpx.ErrInternal(err)
	}
	rows, hasMore := httpx.Paginate(rows, limit)

	items := make([]SessionDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toSessionDTO(row))
	}
	httpx.WriteJSON(w, r, http.StatusOK, httpx.List[SessionDTO]{
		Data: items, Page: httpx.Page{Limit: limit, HasMore: hasMore},
	})
	return nil
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "sessionId")
	if err != nil {
		return err
	}

	session, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("session")
		}
		return httpx.ErrInternal(err)
	}

	dto := SessionDetailDTO{SessionDTO: toSessionDTO(session)}

	if pitcher, err := s.store.GetAthlete(r.Context(), session.PitcherAthleteID); err == nil {
		dto.Pitcher = &SessionRefDTO{
			ID: pitcher.ID, FullName: pitcher.FullName, Throws: pitcher.Throws,
		}
	}

	dist, err := s.store.GetSessionOutcomeDistribution(r.Context(), id)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	if len(dist) > 0 {
		dto.Metrics.OutcomeDistribution = make(map[string]int64, len(dist))
		for _, d := range dist {
			if d.ActualOutcome != nil {
				dto.Metrics.OutcomeDistribution[*d.ActualOutcome] = d.N
			}
		}
	}

	anomalies, err := s.store.ListAnomaliesBySession(r.Context(), id)
	if err != nil {
		return httpx.ErrInternal(err)
	}
	dto.AnomalyCount = int64(len(anomalies))

	httpx.WriteJSON(w, r, http.StatusOK, dto)
	return nil
}

func (s *Server) listSessionPitches(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "sessionId")
	if err != nil {
		return err
	}
	limit, err := httpx.Limit(r, 100, s.cfg.MaxPageSize)
	if err != nil {
		return err
	}
	order, err := httpx.QueryEnum(r, "order", "asc", "desc")
	if err != nil {
		return err
	}
	cursor, err := httpx.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}

	if _, err := s.store.GetSession(r.Context(), id); err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("session")
		}
		return httpx.ErrInternal(err)
	}

	include := httpx.QueryIncludes(r, "measurement", "prediction")
	descending := order != nil && *order == "desc"

	var (
		pitches []dbgen.Pitch
		meas    []dbgen.PitchMeasurement
	)
	if descending {
		rows, err := s.store.ListPitchesBySessionDesc(r.Context(),
			dbgen.ListPitchesBySessionDescParams{
				SessionID: id, Limit: int32(limit + 1), CursorIndex: cursor.Int,
			})
		if err != nil {
			return httpx.ErrInternal(err)
		}
		for _, row := range rows {
			pitches = append(pitches, row.Pitch)
			meas = append(meas, row.PitchMeasurement)
		}
	} else {
		rows, err := s.store.ListPitchesBySession(r.Context(),
			dbgen.ListPitchesBySessionParams{
				SessionID: id, Limit: int32(limit + 1), CursorIndex: cursor.Int,
			})
		if err != nil {
			return httpx.ErrInternal(err)
		}
		for _, row := range rows {
			pitches = append(pitches, row.Pitch)
			meas = append(meas, row.PitchMeasurement)
		}
	}

	pitches, hasMore := httpx.Paginate(pitches, limit)
	items, err := s.buildPitchDTOs(r.Context(), pitches, meas, include)
	if err != nil {
		return err
	}

	page := httpx.Page{Limit: limit, HasMore: hasMore}
	if hasMore && len(pitches) > 0 {
		idx := pitches[len(pitches)-1].SessionPitchIndex
		page.NextCursor = httpx.Cursor{Int: &idx}.Encode()
	}

	httpx.WriteJSON(w, r, http.StatusOK, httpx.List[PitchDTO]{Data: items, Page: page})
	return nil
}

// listPitches is the cross-session search.
//
// pitcher_id is required, and that is a design decision rather than an
// oversight: without it the query is a sequential scan over millions of rows.
// Making the expensive query impossible to express is cheaper than
// discovering it in production and optimizing afterwards.
func (s *Server) listPitches(w http.ResponseWriter, r *http.Request) error {
	pitcherID, err := httpx.QueryUUID(r, "pitcher_id")
	if err != nil {
		return err
	}
	limit, err := httpx.Limit(r, s.cfg.DefaultPageSize, s.cfg.MaxPageSize)
	if err != nil {
		return err
	}
	outcome, err := httpx.QueryEnum(r, "outcome",
		"BALL", "CALLED_STRIKE", "SWINGING_STRIKE", "FOUL", "IN_PLAY")
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
	minSpeed, err := httpx.QueryFloat32(r, "min_speed")
	if err != nil {
		return err
	}
	maxSpeed, err := httpx.QueryFloat32(r, "max_speed")
	if err != nil {
		return err
	}
	if minSpeed != nil && maxSpeed != nil && *maxSpeed < *minSpeed {
		return httpx.ErrValidation("max_speed must not be below min_speed",
			httpx.FieldError{Field: "max_speed", Message: "below min_speed"})
	}
	cursor, err := httpx.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}

	params := dbgen.ListPitchesByPitcherParams{
		PitcherAthleteID: pitcherID, Limit: int32(limit + 1),
		PitchType: httpx.QueryString(r, "pitch_type"), Outcome: outcome,
		FromTs: from, ToTs: to, MinSpeed: minSpeed, MaxSpeed: maxSpeed,
	}
	if cursor.Time != nil && cursor.ID != nil {
		params.CursorThrown = cursor.Time
		params.CursorID = cursor.ID
	}

	rows, err := s.store.ListPitchesByPitcher(r.Context(), params)
	if err != nil {
		return httpx.ErrInternal(err)
	}

	var (
		pitches []dbgen.Pitch
		meas    []dbgen.PitchMeasurement
	)
	for _, row := range rows {
		pitches = append(pitches, row.Pitch)
		meas = append(meas, row.PitchMeasurement)
	}

	pitches, hasMore := httpx.Paginate(pitches, limit)
	items, err := s.buildPitchDTOs(r.Context(), pitches, meas,
		httpx.QueryIncludes(r, "measurement", "prediction"))
	if err != nil {
		return err
	}

	page := httpx.Page{Limit: limit, HasMore: hasMore}
	if hasMore && len(pitches) > 0 {
		last := pitches[len(pitches)-1]
		page.NextCursor = httpx.Cursor{Time: &last.ThrownAt, ID: &last.ID}.Encode()
	}

	httpx.WriteJSON(w, r, http.StatusOK, httpx.List[PitchDTO]{Data: items, Page: page})
	return nil
}

func (s *Server) getPitch(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "pitchId")
	if err != nil {
		return err
	}

	pitch, err := s.store.GetPitch(r.Context(), id)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("pitch")
		}
		return httpx.ErrInternal(err)
	}

	measurement, err := s.store.GetPitchMeasurement(r.Context(), id)
	if err != nil && !db.IsNoRows(err) {
		return httpx.ErrInternal(err)
	}

	dto := toPitchDTO(pitch, measurement)

	preds, err := s.store.ListPredictionsForPitch(r.Context(),
		dbgen.ListPredictionsForPitchParams{PitchID: id})
	if err != nil {
		return httpx.ErrInternal(err)
	}
	for _, p := range preds {
		dto.Predictions = append(dto.Predictions, toPredictionDTO(p.PitchPrediction, p.ModelVersion))
	}

	httpx.WriteJSON(w, r, http.StatusOK, dto)
	return nil
}

// listPitchPredictions returns every prediction ever made for a pitch.
//
// Old predictions are not deleted when a model is retrained, so this is what
// makes "is v1.1 actually better than v1.0" answerable on the same pitch
// rather than only in aggregate.
func (s *Server) listPitchPredictions(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "pitchId")
	if err != nil {
		return err
	}
	variant, err := httpx.QueryEnum(r, "variant", "stuff", "pitching")
	if err != nil {
		return err
	}

	if _, err := s.store.GetPitch(r.Context(), id); err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("pitch")
		}
		return httpx.ErrInternal(err)
	}

	rows, err := s.store.ListPredictionsForPitch(r.Context(),
		dbgen.ListPredictionsForPitchParams{
			PitchID: id, Variant: variant, Version: httpx.QueryString(r, "model_version"),
		})
	if err != nil {
		return httpx.ErrInternal(err)
	}

	items := make([]PredictionDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toPredictionDTO(row.PitchPrediction, row.ModelVersion))
	}

	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{
		"pitch_id": id, "data": items,
	})
	return nil
}

// closeSession is an action endpoint rather than a PATCH on status.
//
// It is a state machine transition with side effects: it triggers anomaly
// evaluation and lets the live session state expire. PATCH {"status": ...}
// would hide that behind what looks like a field update.
func (s *Server) closeSession(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "sessionId")
	if err != nil {
		return err
	}

	_, err = s.store.CloseSession(r.Context(), id)
	if err == nil {
		if _, err := s.store.RecomputeSessionAggregates(r.Context(), id); err != nil {
			// The session is closed either way; stale averages are a
			// cosmetic problem, not a reason to report failure.
			httpx.LoggerFrom(r.Context()).Warn("recompute session aggregates",
				"session_id", id, "error", err)
		}
		refreshed, err := s.store.GetSession(r.Context(), id)
		if err != nil {
			return httpx.ErrInternal(err)
		}
		httpx.WriteJSON(w, r, http.StatusOK, toSessionDTO(refreshed))
		return nil
	}
	if !db.IsNoRows(err) {
		return httpx.ErrInternal(err)
	}

	if _, getErr := s.store.GetSession(r.Context(), id); getErr != nil {
		if db.IsNoRows(getErr) {
			return httpx.ErrNotFound("session")
		}
		return httpx.ErrInternal(getErr)
	}
	return httpx.ErrConflict("session is not active")
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) error {
	variant, err := httpx.QueryEnum(r, "variant", "stuff", "pitching")
	if err != nil {
		return err
	}
	active, err := httpx.QueryBool(r, "active")
	if err != nil {
		return err
	}

	rows, err := s.store.ListModelVersions(r.Context(),
		dbgen.ListModelVersionsParams{Variant: variant, Active: active})
	if err != nil {
		return httpx.ErrInternal(err)
	}

	items := make([]ModelVersionDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toModelVersionDTO(row))
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"data": items})
	return nil
}

// buildPitchDTOs assembles pitch responses, optionally attaching the active
// models' predictions.
func (s *Server) buildPitchDTOs(
	ctx context.Context,
	pitches []dbgen.Pitch,
	meas []dbgen.PitchMeasurement,
	include map[string]bool,
) ([]PitchDTO, error) {
	items := make([]PitchDTO, 0, len(pitches))

	// Predictions are fetched for the whole page in one query. Looking them
	// up per pitch would issue one round trip per row, which is the classic
	// N+1: invisible on a fixture, ruinous on a 500-pitch page.
	byPitch := map[uuid.UUID][]PredictionDTO{}
	if include["prediction"] && len(pitches) > 0 {
		ids := make([]uuid.UUID, 0, len(pitches))
		for _, p := range pitches {
			ids = append(ids, p.ID)
		}
		rows, err := s.store.GetActivePredictionsForPitches(ctx, ids)
		if err != nil {
			return nil, httpx.ErrInternal(err)
		}
		for _, row := range rows {
			byPitch[row.PitchPrediction.PitchID] = append(
				byPitch[row.PitchPrediction.PitchID],
				toPredictionDTO(row.PitchPrediction, row.ModelVersion))
		}
	}

	for i, p := range pitches {
		var m dbgen.PitchMeasurement
		if include["measurement"] && i < len(meas) {
			m = meas[i]
		}
		dto := toPitchDTO(p, m)
		dto.Predictions = byPitch[p.ID]
		items = append(items, dto)
	}
	return items, nil
}
