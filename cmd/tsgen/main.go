// Command tsgen writes the dashboard's TypeScript types from the Go response
// structs.
//
//	make ts-types
//
// The generated file is committed. A test compares it against a fresh
// generation, so a change to a response shape that is not accompanied by a
// regeneration fails the build rather than the browser.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hkocamandev/pitchlab/internal/api"
	"github.com/hkocamandev/pitchlab/internal/events"
	"github.com/hkocamandev/pitchlab/internal/httpx"
	"github.com/hkocamandev/pitchlab/internal/ws"
)

// DefaultOutput is where the dashboard reads its types from.
const DefaultOutput = "web/src/api/types.gen.ts"

// entries lists every type the dashboard receives.
//
// Explicit rather than discovered by scanning the package: a response type is
// part of the public surface, and adding one should be a decision someone
// makes, not a side effect of declaring a struct.
func entries() []entry {
	return []entry{
		{"AthleteDTO", api.AthleteDTO{}},
		{"AthleteTotalsDTO", api.AthleteTotalsDTO{}},
		{"AthleteListItemDTO", api.AthleteListItemDTO{}},
		{"AthleteDetailDTO", api.AthleteDetailDTO{}},

		{"SessionRefDTO", api.SessionRefDTO{}},
		{"SessionMetricsDTO", api.SessionMetricsDTO{}},
		{"SessionDTO", api.SessionDTO{}},
		{"SessionDetailDTO", api.SessionDetailDTO{}},

		{"PitchContextDTO", api.PitchContextDTO{}},
		{"MeasurementDTO", api.MeasurementDTO{}},
		{"PredictionDTO", api.PredictionDTO{}},
		{"PitchDTO", api.PitchDTO{}},

		{"AnomalyDTO", api.AnomalyDTO{}},

		{"ModelPeriodDTO", api.ModelPeriodDTO{}},
		{"ModelVersionDTO", api.ModelVersionDTO{}},

		{"AthleteSummaryDTO", api.AthleteSummaryDTO{}},
		{"PitchTypeStatDTO", api.PitchTypeStatDTO{}},
		{"TrendPointDTO", api.TrendPointDTO{}},
		{"AnalyticsDTO", api.AnalyticsDTO{}},

		{"FeatureContributionDTO", api.FeatureContributionDTO{}},
		{"ExplanationDTO", api.ExplanationDTO{}},

		// Live payloads. The dashboard receives these over the WebSocket, and
		// they are deliberately a different shape from the REST pitch: they are
		// built from the Kafka event rather than read back from the database.
		//
		// The event types are registered under Live* names because their Go
		// names collide with the REST ones while describing different shapes.
		// Letting them share a TypeScript name would silently merge two
		// contracts.
		{"LivePitchContext", events.PitchContext{}},
		{"LiveMeasurement", events.NormalizedMeasurement{}},
		{"LivePrediction", events.PredictionResult{}},
		{"LiveAnomaly", events.AnomalyDetected{}},
		{"LivePitchDTO", api.LivePitchDTO{}},
		{"LiveMetricsDTO", api.LiveMetricsDTO{}},

		// Envelopes.
		{"PageDTO", httpx.Page{}},
		{"WSMessageDTO", ws.Message{}},
		{"WSErrorDTO", ws.ErrorData{}},
		{"WSClosedDTO", ws.ClosedData{}},
	}
}

func main() {
	out := flag.String("out", DefaultOutput, "file to write")
	check := flag.Bool("check", false,
		"exit non-zero if the file on disk is out of date, and write nothing")
	flag.Parse()

	generated := Generate(entries())

	if *check {
		current, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", *out, err)
			os.Exit(1)
		}
		if string(current) != generated {
			fmt.Fprintf(os.Stderr,
				"%s is out of date; run: make ts-types\n", *out)
			os.Exit(1)
		}
		return
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(generated), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s (%d types)\n", *out, len(entries()))
}
