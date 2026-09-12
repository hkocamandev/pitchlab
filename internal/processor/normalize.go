// Package processor turns raw device readings into scored, persisted pitches.
package processor

import (
	"math"

	"github.com/hkocamandev/pitchlab/internal/events"
)

// NormalizationVersion identifies this transformation pass.
//
// Stored on every measurement row. Without it, a partial re-normalization
// leaves the table holding two conventions at once with no way to tell which
// row follows which.
const NormalizationVersion = 1

// PlateHalfWidthFt is half of home plate: 17 inches.
const PlateHalfWidthFt = 17.0 / 2.0 / 12.0

// measurementBounds mirror the CHECK constraints on pitch_measurements, so a
// fault is caught before the insert rather than as a constraint violation
// that dead-letters an otherwise usable pitch.
//
// Generous on purpose: the intent is to catch sensor faults, not to reject
// unusual but real pitches.
var measurementBounds = map[string][2]float64{
	"release_speed":     {40, 110},
	"release_spin_rate": {0, 4000},
	"spin_axis":         {0, 360},
	"release_extension": {3, 9},
	"release_pos_z":     {0, 8},
	"release_pos_x":     {-6, 6},
	"pfx_x":             {-3, 3},
	"pfx_z":             {-3, 3},
	"plate_x":           {-4, 4},
	"plate_z":           {-3, 8},
	"sz_top":            {2, 5},
	"sz_bot":            {0.5, 3},
}

// Normalize applies the domain transformation rules to a raw reading.
//
// These rules live here, in the processor, rather than in whatever produced
// the reading. A tracking camera does not know about handedness conventions,
// and putting the rules on the device side would mean every future device
// reimplemented them.
//
// The transformations must agree exactly with the Python feature code the
// models were trained on. A silent divergence would not error anywhere; the
// scores would simply stop meaning anything.
func Normalize(raw events.RawMeasurement, pThrows, stand string) events.NormalizedMeasurement {
	out := events.NormalizedMeasurement{
		RawMeasurement:       clampBounds(raw),
		NormalizationVersion: NormalizationVersion,
		MeasurementQuality:   quality(raw),
	}

	isRHP := pThrows == "R"
	isRHB := stand == "R"

	// Mirror horizontal quantities into a handedness-neutral frame where
	// positive means arm side for a pitcher and inside for a batter.
	//
	// The direction was established from data, not from geometry -- see
	// ml/scripts/verify_plate_sign.py. Right-handed batters are hit by
	// pitches at a mean plate_x of -1.94 and left-handers at +1.98, and
	// four-seam fastballs average -0.63 pfx_x for RHP against +0.66 for LHP.
	// Reasoning about it from the diamond gets it backwards, which is how the
	// first implementation shipped inverted.
	out.PfxXArm = mirrorIf(isRHP, out.PfxX)
	out.ReleasePosXArm = mirrorIf(isRHP, out.ReleasePosX)
	out.PlateXBat = mirrorIf(isRHB, out.PlateX)

	// Express height as a fraction of this batter's own zone, so a tall and a
	// short batter are comparable.
	if out.PlateZ != nil && out.SzTop != nil && out.SzBot != nil {
		height := *out.SzTop - *out.SzBot
		if height > 0.5 { // guard against a bad zone marking
			norm := (*out.PlateZ - *out.SzBot) / height
			out.ZoneHeightNorm = &norm
		}
	}

	if out.PlateX != nil && out.PlateZ != nil && out.SzTop != nil && out.SzBot != nil {
		inZone := math.Abs(*out.PlateX) <= PlateHalfWidthFt &&
			*out.PlateZ >= *out.SzBot && *out.PlateZ <= *out.SzTop
		out.InZone = &inZone
	}

	return out
}

// mirrorIf negates a value when the subject is right-handed.
func mirrorIf(isRight bool, v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	if isRight {
		out = -out
	}
	return &out
}

// clampBounds nulls physically impossible readings.
//
// Null rather than drop: one bad sensor field should not discard an otherwise
// usable pitch, and the model handles missing values natively. Substituting a
// plausible number instead would invent a measurement nobody took.
func clampBounds(raw events.RawMeasurement) events.RawMeasurement {
	out := raw
	out.ReleaseSpeed = withinBounds("release_speed", out.ReleaseSpeed)
	out.ReleaseSpinRate = withinBounds("release_spin_rate", out.ReleaseSpinRate)
	out.SpinAxis = withinBounds("spin_axis", out.SpinAxis)
	out.ReleaseExtension = withinBounds("release_extension", out.ReleaseExtension)
	out.ReleasePosX = withinBounds("release_pos_x", out.ReleasePosX)
	out.ReleasePosZ = withinBounds("release_pos_z", out.ReleasePosZ)
	out.PfxX = withinBounds("pfx_x", out.PfxX)
	out.PfxZ = withinBounds("pfx_z", out.PfxZ)
	out.PlateX = withinBounds("plate_x", out.PlateX)
	out.PlateZ = withinBounds("plate_z", out.PlateZ)
	out.SzTop = withinBounds("sz_top", out.SzTop)
	out.SzBot = withinBounds("sz_bot", out.SzBot)
	return out
}

func withinBounds(field string, v *float64) *float64 {
	if v == nil {
		return nil
	}
	b, ok := measurementBounds[field]
	if !ok {
		return v
	}
	if math.IsNaN(*v) || *v < b[0] || *v > b[1] {
		return nil
	}
	return v
}

// quality grades how complete a reading is.
//
// The grade is recorded rather than acted on here: an analytics query can
// exclude SUSPECT rows, and the anomaly baseline does, but a partial reading
// is still a pitch that happened and is still worth storing.
func quality(raw events.RawMeasurement) string {
	core := []*float64{raw.ReleaseSpeed, raw.PlateX, raw.PlateZ}
	missing := 0
	for _, v := range core {
		if v == nil {
			missing++
		}
	}
	switch {
	case missing == len(core):
		return "SUSPECT"
	case missing > 0:
		return "PARTIAL"
	default:
		return "OK"
	}
}

// CountState renders the pre-pitch count the way Statcast writes it. The ML
// service uses it to look up run values, so the format is a contract.
func CountState(balls, strikes int) string {
	return string(rune('0'+balls)) + "-" + string(rune('0'+strikes))
}

// RunnersState encodes occupied bases as a three-character pattern.
func RunnersState(on1b, on2b, on3b bool) string {
	out := []byte("___")
	if on1b {
		out[0] = '1'
	}
	if on2b {
		out[1] = '2'
	}
	if on3b {
		out[2] = '3'
	}
	return string(out)
}

// spinAxisComponents projects a spin axis onto the unit circle.
//
// Statcast reports the axis in degrees, 0 to 360, where 180 is pure backspin
// and 0 is pure topspin. Feeding that number to a model directly teaches it
// that 359 and 1 are far apart, which is false: they are two degrees apart.
//
// Mirroring for handedness flips the sine and leaves the cosine untouched,
// since sin(360-t) = -sin(t) and cos(360-t) = cos(t).
func spinAxisComponents(degrees float64, isRHP bool) (sin, cos float64) {
	rad := degrees * math.Pi / 180.0
	sin, cos = math.Sin(rad), math.Cos(rad)
	if isRHP {
		sin = -sin
	}
	return sin, cos
}

// distFromZoneCenter measures how far a pitch missed the middle of the zone.
//
// Both axes are scaled to the zone, so the distance means the same thing for a
// tall batter and a short one. This ends up carrying more of the outcome
// model's signal than any other feature -- which is geometry, not leakage: a
// pitch two feet outside is a ball, and that is a property of the ball's own
// trajectory rather than of what happened next.
func distFromZoneCenter(plateX, zoneHeightNorm float64) float64 {
	dx := plateX / PlateHalfWidthFt
	dz := (zoneHeightNorm - 0.5) * 2.0
	return math.Sqrt(dx*dx + dz*dz)
}
