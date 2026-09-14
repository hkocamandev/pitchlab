package api

import (
	"net/http"

	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/db/dbgen"
	"github.com/hkocamandev/pitchlab/internal/events"
	"github.com/hkocamandev/pitchlab/internal/httpx"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
	"github.com/hkocamandev/pitchlab/internal/processor"
)

// FeatureContributionDTO is one feature's share of a prediction.
type FeatureContributionDTO struct {
	Feature string   `json:"feature"`
	Value   *float64 `json:"value"`
	// Contribution is in the model's margin space, not in probability. A
	// gradient-boosted model is additive in log-odds and not after the
	// softmax, so these numbers sum to the margin -- reporting them as
	// percentage points of probability would be arithmetic that does not hold.
	Contribution float64 `json:"contribution"`
}

// ExplanationDTO is one pitch's attribution.
type ExplanationDTO struct {
	PitchID      string `json:"pitch_id"`
	Variant      string `json:"variant"`
	ModelVersion string `json:"model_version"`
	Target       string `json:"target"`

	PredictedClass string             `json:"predicted_class,omitempty"`
	Probabilities  map[string]float64 `json:"probabilities,omitempty"`

	// TargetClass names which output is being explained, and Space says in
	// what units. Both are required to read the numbers below: a multiclass
	// model has one attribution per class, and they are additive in the
	// margin rather than after the softmax.
	TargetClass string `json:"target_class"`
	Space       string `json:"space"`

	// BaseValue is the model's average output and PredictedValue is this
	// pitch's; the contributions explain the difference between them.
	// PredictedProbability is carried alongside because the margin is not
	// something anyone reads directly.
	BaseValue            float64 `json:"base_value"`
	PredictedValue       float64 `json:"predicted_value"`
	PredictedProbability float64 `json:"predicted_probability"`
	// OtherContribution is everything outside the top features, so the parts
	// add up and the list cannot be mistaken for the whole explanation.
	OtherContribution float64 `json:"other_contribution"`

	Contributions []FeatureContributionDTO `json:"contributions"`
	InferenceMs   float64                  `json:"inference_ms"`
}

// getPitchExplanation attributes one pitch's score to its features.
//
// A separate endpoint, computed on request, because attribution costs a model
// evaluation per feature. Attaching it to every pitch would put that cost on
// the live path for data nobody has asked to see; a coach asks about one pitch
// at a time, and computing it then is the right trade.
//
// The feature vector is rebuilt with the same code the stream processor uses
// rather than a copy written here. Two implementations of the same feature
// construction would drift, and the failure would be silent: an explanation of
// a slightly different pitch than the one that was scored.
func (s *Server) getPitchExplanation(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "pitchId")
	if err != nil {
		return err
	}
	variantParam, err := httpx.QueryEnum(r, "variant", "pitching", "stuff")
	if err != nil {
		return err
	}
	variant := mlclient.VariantPitching
	if variantParam != nil && *variantParam == "stuff" {
		variant = mlclient.VariantStuff
	}

	if s.ml == nil {
		// Configured without an inference client. Unavailable is the honest
		// answer; a 500 would suggest something broke.
		return httpx.ErrUnavailable("explanations are not enabled on this instance", nil)
	}

	pitch, err := s.store.GetPitch(r.Context(), id)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound("pitch")
		}
		return httpx.ErrInternal(err)
	}

	measurement, err := s.store.GetPitchMeasurement(r.Context(), id)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrUnprocessable(
				"this pitch has no measurement, so there is nothing to attribute")
		}
		return httpx.ErrInternal(err)
	}

	input := featureInputFrom(pitch, measurement)
	features := processor.BuildFeatures(input, variant)

	resp, err := s.ml.Explain(r.Context(), variant, mlclient.PitchFeatures{
		PitchID:    id.String(),
		CountState: processor.CountState(int(pitch.Balls), int(pitch.Strikes)),
		Features:   features,
	}, 8)
	if err != nil {
		// The inference service being down is not this API being broken.
		return httpx.ErrUnavailable("the inference service is unavailable", err)
	}
	if len(resp.Predictions) == 0 {
		return httpx.ErrInternal(nil)
	}

	p := resp.Predictions[0]
	out := ExplanationDTO{
		PitchID:      id.String(),
		Variant:      resp.Variant,
		ModelVersion: resp.ModelVersion,
		Target:       resp.Target,
		InferenceMs:  resp.InferenceMs,
	}
	if p.Explanation != nil {
		out.TargetClass = p.Explanation.TargetClass
		out.Space = p.Explanation.Space
		out.BaseValue = p.Explanation.BaseValue
		out.PredictedValue = p.Explanation.PredictedValue
		out.PredictedProbability = p.Explanation.PredictedProbability
		out.OtherContribution = p.Explanation.OtherContribution
		for _, c := range p.Explanation.Contributions {
			out.Contributions = append(out.Contributions, FeatureContributionDTO{
				Feature: c.Feature, Value: c.Value, Contribution: c.SHAP,
			})
		}
	}
	if p.PredictedClass != nil {
		out.PredictedClass = *p.PredictedClass
	}
	out.Probabilities = p.Probabilities

	httpx.WriteJSON(w, r, http.StatusOK, out)
	return nil
}

// featureInputFrom rebuilds the processor's input from the stored rows.
//
// Everything the feature builder reads was persisted at ingest time, which is
// what makes this reconstruction exact rather than approximate. The
// arsenal-relative fields are left unset here because they are unset on the
// live path too -- an explanation has to describe the model that ran, not a
// better one.
func featureInputFrom(p dbgen.Pitch, m dbgen.PitchMeasurement) processor.FeatureInput {
	raw := events.RawMeasurement{
		ReleaseSpeed:     f64(m.ReleaseSpeed),
		ReleaseSpinRate:  f64(m.ReleaseSpinRate),
		SpinAxis:         f64(m.SpinAxis),
		ReleasePosX:      f64(m.ReleasePosX),
		ReleasePosZ:      f64(m.ReleasePosZ),
		ReleaseExtension: f64(m.ReleaseExtension),
		PfxX:             f64(m.PfxX),
		PfxZ:             f64(m.PfxZ),
		PlateX:           f64(m.PlateX),
		PlateZ:           f64(m.PlateZ),
		SzTop:            f64(m.SzTop),
		SzBot:            f64(m.SzBot),
		ArmAngle:         f64(m.ArmAngle),
	}
	if p.PitchType != nil {
		raw.PitchType = *p.PitchType
	}

	return processor.FeatureInput{
		Measurement: events.NormalizedMeasurement{
			RawMeasurement:       raw,
			PfxXArm:              f64(m.PfxXArm),
			ReleasePosXArm:       f64(m.ReleasePosXArm),
			PlateXBat:            f64(m.PlateXBat),
			ZoneHeightNorm:       f64(m.ZoneHeightNorm),
			InZone:               m.InZone,
			NormalizationVersion: int(m.NormalizationVersion),
			MeasurementQuality:   m.MeasurementQuality,
		},
		Context: events.PitchContext{
			Balls:      int(p.Balls),
			Strikes:    int(p.Strikes),
			OutsWhenUp: int(p.OutsWhenUp),
			Inning:     int(p.Inning),
			On1B:       p.On1b,
			On2B:       p.On2b,
			On3B:       p.On3b,
		},
		Stand:       p.Stand,
		PThrows:     p.PThrows,
		PitchType:   raw.PitchType,
		PitchNumber: int(p.PitchNumber),
	}
}

// f64 widens a stored float32 to the float64 the feature builder works in.
func f64(v *float32) *float64 {
	if v == nil {
		return nil
	}
	out := float64(*v)
	return &out
}
