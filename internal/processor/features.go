package processor

import (
	"github.com/hkocamandev/pitchlab/internal/events"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
)

// pitchGroup maps a pitch type to its coarse family.
var pitchGroup = map[string]string{
	"FF": "FASTBALL", "SI": "FASTBALL", "FC": "FASTBALL", "FA": "FASTBALL",
	"SL": "BREAKING", "ST": "BREAKING", "SV": "BREAKING",
	"CU": "BREAKING", "KC": "BREAKING", "CS": "BREAKING", "SC": "BREAKING",
	"CH": "OFFSPEED", "FS": "OFFSPEED", "FO": "OFFSPEED",
	"KN": "OTHER", "EP": "OTHER", "PO": "OTHER", "IN": "OTHER",
}

// FeatureInput is everything the feature builder is allowed to see.
//
// This struct is the leakage defense in the Go path, and its shape is the
// point: there is no field for the outcome. `pitches.actual_outcome` exists
// and is written, but it cannot reach a feature vector because the function
// that builds one cannot be handed it. The whitelist is enforced by the type
// system rather than by remembering.
type FeatureInput struct {
	Measurement events.NormalizedMeasurement
	Context     events.PitchContext
	Stand       string
	PThrows     string
	PitchType   string

	// Arsenal-relative values are computed from the pitcher's *previous*
	// season and passed in. Computing them from the current season would let
	// a season aggregate be visible to the first pitch of that season.
	VeloDiffVsFB      *float64
	PfxXDiffVsFB      *float64
	PfxZDiffVsFB      *float64
	ReleaseDistFromFB *float64

	// PitchNumber is the pitch's index within the plate appearance, and
	// DaysSincePrevGame the pitcher's rest. Both are situation, not shape, so
	// they belong with the context features and never reach the stuff model.
	PitchNumber   int
	DaysSincePrev *float64
}

// BuildFeatures assembles the map sent to the inference service.
//
// Missing values are omitted rather than sent as zero or as an imputed mean.
// The model handles absence natively, and the absence of a reading is itself
// information; filling it in would invent a measurement the device never took.
//
// The inference service validates these keys against the model's own feature
// list and rejects anything unrecognised, so a name that drifts here fails
// loudly instead of silently arriving as a missing value.
func BuildFeatures(in FeatureInput, variant mlclient.Variant) map[string]any {
	m := in.Measurement
	f := make(map[string]any, 34)

	// Physical characteristics -- present in both variants.
	putFloat(f, "release_speed", m.ReleaseSpeed)
	putFloat(f, "release_spin_rate", m.ReleaseSpinRate)
	putFloat(f, "pfx_x_arm", m.PfxXArm)
	putFloat(f, "pfx_z", m.PfxZ)
	putFloat(f, "release_extension", m.ReleaseExtension)
	putFloat(f, "release_pos_z", m.ReleasePosZ)
	putFloat(f, "release_pos_x_arm", m.ReleasePosXArm)
	putFloat(f, "arm_angle", m.ArmAngle)

	// Spin axis is circular: 359 degrees and 1 degree are two degrees apart,
	// not 358. Feeding the raw number would teach the model a discontinuity
	// that does not exist, so it is projected onto the unit circle. Mirroring
	// for handedness flips the sine and leaves the cosine, since
	// sin(360-t) = -sin(t) and cos(360-t) = cos(t).
	if m.SpinAxis != nil {
		sin, cos := spinAxisComponents(*m.SpinAxis, in.PThrows == "R")
		f["spin_axis_sin"] = sin
		f["spin_axis_cos"] = cos
	}

	pitchType := in.PitchType
	if pitchType == "" {
		// An unclassified pitch is still a pitch. Dropping the row would
		// quietly bias the training set toward whatever the classifier finds
		// easy, so UNKNOWN is a category of its own.
		pitchType = "UNKNOWN"
	}
	f["pitch_type"] = pitchType
	group, ok := pitchGroup[pitchType]
	if !ok {
		group = "OTHER"
	}
	f["pitch_group"] = group

	f["stand"] = in.Stand
	f["p_throws"] = in.PThrows
	// The platoon matchup is modelled explicitly rather than left for the
	// model to discover from two separate categoricals.
	f["same_handed"] = boolToInt(in.Stand == in.PThrows)

	putFloat(f, "velo_diff_vs_fb", in.VeloDiffVsFB)
	putFloat(f, "pfx_x_diff_vs_fb", in.PfxXDiffVsFB)
	putFloat(f, "pfx_z_diff_vs_fb", in.PfxZDiffVsFB)
	putFloat(f, "release_dist_from_fb", in.ReleaseDistFromFB)

	// The stuff model sees the pitch's shape only: not where it went, and not
	// the game situation. Scoring "stuff" on anything else buries it under
	// geometry, which is what phase 1 measured.
	if variant == mlclient.VariantStuff {
		return f
	}

	// Game situation. Statcast documents every one of these as pre-pitch,
	// which is what makes them safe.
	c := in.Context
	f["balls"] = c.Balls
	f["strikes"] = c.Strikes
	f["count_state"] = CountState(c.Balls, c.Strikes)
	f["outs_when_up"] = c.OutsWhenUp
	f["runners_state"] = RunnersState(c.On1B, c.On2B, c.On3B)
	f["inning"] = c.Inning
	if c.NThruOrderPitcher != nil {
		f["n_thruorder_pitcher"] = *c.NThruOrderPitcher
	}
	if c.BatScore != nil && c.FldScore != nil {
		f["score_diff"] = *c.BatScore - *c.FldScore
	}
	f["pitch_number"] = in.PitchNumber
	putFloat(f, "pitcher_days_since_prev_game", in.DaysSincePrev)

	// Location.
	putFloat(f, "plate_x_bat", m.PlateXBat)
	putFloat(f, "zone_height_norm", m.ZoneHeightNorm)
	if m.InZone != nil {
		f["in_zone"] = boolToInt(*m.InZone)
	}
	if m.PlateX != nil && m.ZoneHeightNorm != nil {
		f["dist_from_zone_center"] = distFromZoneCenter(*m.PlateX, *m.ZoneHeightNorm)
	}

	return f
}

func putFloat(f map[string]any, key string, v *float64) {
	if v != nil {
		f[key] = *v
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
