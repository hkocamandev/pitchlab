import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { Measurement, Pitch, PitchTypeStat } from "../api/types.gen";
import { Empty, ErrorState, Kpi, Query, SyntheticBadge } from "./common";
import { ApiError } from "../api/client";
import { MeasurementTable, ProbabilityBars } from "./PitchDetail";
import { StrikeZone } from "./StrikeZone";
import { PitchTypeBars } from "./charts";

function measurement(over: Partial<Measurement> = {}): Measurement {
  return {
    release_speed: 95,
    release_spin_rate: 2400,
    spin_axis: 210,
    pfx_x: -0.53,
    pfx_z: 1.4,
    plate_x: 0.2,
    plate_z: 2.4,
    release_extension: 6.4,
    release_pos_x: -1.9,
    release_pos_z: 5.8,
    arm_angle: 42,
    pfx_x_arm: 0.53,
    plate_x_bat: -0.2,
    zone_height_norm: 0.5,
    in_zone: true,
    measurement_quality: "OK",
    ...over,
  };
}

function pitch(over: Partial<Pitch> = {}): Pitch {
  return {
    id: "p1",
    session_id: "s1",
    session_pitch_index: 12,
    at_bat_number: 4,
    pitch_number: 2,
    thrown_at: "2025-08-20T19:47:30Z",
    context: {
      balls: 1,
      strikes: 2,
      outs_when_up: 1,
      inning: 5,
      inning_topbot: "Top",
      stand: "L",
      p_throws: "R",
      on_1b: false,
      on_2b: false,
      on_3b: false,
    },
    pitch_type: "SL",
    pitch_name: "Slider",
    measurement: measurement(),
    predictions: [],
    actual_outcome: "SWINGING_STRIKE",
    prediction_status: "OK",
    ...over,
  };
}

// --- T9.1 components ------------------------------------------------------

describe("Kpi", () => {
  it("distinguishes no measurement from zero", () => {
    const { rerender } = render(<Kpi label="Avg spin" value={null} unit="rpm" />);
    // A pitcher with no spin readings has no average spin. Printing 0 rpm
    // would be a measurement nobody made.
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.queryByText("0.0")).not.toBeInTheDocument();

    rerender(<Kpi label="Avg spin" value={0} unit="rpm" />);
    expect(screen.getByText("0.0")).toBeInTheDocument();
  });
});

describe("SyntheticBadge", () => {
  it("marks replayed data and stays out of the way otherwise", () => {
    const { rerender } = render(<SyntheticBadge synthetic />);
    // A viewer has to be able to tell a simulation from live play without
    // reading the source.
    expect(screen.getByText("synthetic")).toBeInTheDocument();

    rerender(<SyntheticBadge synthetic={false} />);
    expect(screen.queryByText("synthetic")).not.toBeInTheDocument();
  });
});

describe("ProbabilityBars", () => {
  it("shows the model's distribution next to what actually happened", () => {
    render(
      <ProbabilityBars
        probabilities={{
          BALL: 0.18,
          CALLED_STRIKE: 0.05,
          SWINGING_STRIKE: 0.41,
          FOUL: 0.22,
          IN_PLAY: 0.14,
        }}
        actual="FOUL"
      />,
    );

    expect(screen.getByText("41.0%")).toBeInTheDocument();

    // The actual outcome is marked even though the model favoured another
    // class. Showing the pitches the model got wrong is the honest version.
    expect(screen.getByText("← actual")).toBeInTheDocument();
    const foulRow = screen.getByText("Foul").closest(".prob-row");
    expect(foulRow).toHaveClass("actual");
  });

  it("renders every class, including ones the model gave no weight", () => {
    render(<ProbabilityBars probabilities={{ BALL: 1 }} actual="BALL" />);

    // A distribution with classes missing from the chart reads as if those
    // outcomes were impossible rather than merely unlikely.
    for (const label of ["Ball", "Called strike", "Whiff", "Foul", "In play"]) {
      expect(screen.getByText(new RegExp(`^${label}`))).toBeInTheDocument();
    }
  });
});

describe("MeasurementTable", () => {
  it("shows the raw reading and the arm-frame value side by side", () => {
    render(<MeasurementTable m={measurement()} />);

    // The raw value is what the device produced; the arm-frame value is what
    // makes left- and right-handers comparable. Showing only one of them
    // would hide the normalization.
    expect(screen.getByText("Horizontal break (raw)")).toBeInTheDocument();
    expect(screen.getByText("Horizontal break (arm-side)")).toBeInTheDocument();
    expect(screen.getByText(/-0\.53/)).toBeInTheDocument();
    expect(screen.getByText(/^0\.53/)).toBeInTheDocument();
  });

  it("renders a missing reading as absent, not as zero", () => {
    render(<MeasurementTable m={measurement({ release_spin_rate: null })} />);
    const row = screen.getByText("Spin rate").closest("tr");
    expect(within(row!).getByText("—")).toBeInTheDocument();
  });
});

describe("StrikeZone", () => {
  it("plots located pitches and selects on click", async () => {
    const onSelect = vi.fn();
    render(
      <StrikeZone
        pitches={[pitch(), pitch({ id: "p2", session_pitch_index: 13 })]}
        selectedId="p1"
        onSelect={onSelect}
      />,
    );

    const plot = screen.getByRole("img", { name: /catcher/i });
    expect(plot.querySelectorAll("circle")).toHaveLength(2);

    await userEvent.click(plot.querySelectorAll("circle")[1]!);
    expect(onSelect).toHaveBeenCalledWith("p2");
  });

  it("says so when no pitch has a location", () => {
    render(
      <StrikeZone
        pitches={[pitch({ measurement: measurement({ plate_x: null, plate_z: null }) })]}
      />,
    );
    expect(screen.getByText(/no location readings/i)).toBeInTheDocument();
  });
});

describe("PitchTypeBars", () => {
  it("orders by usage", () => {
    const stats: PitchTypeStat[] = [
      {
        pitch_type: "SL",
        pitch_name: "Slider",
        count: 20,
        usage_pct: 0.2,
        avg_release_speed: 87,
        avg_release_spin_rate: 2500,
        avg_pfx_x_arm: 0.2,
        avg_pfx_z: -0.1,
        avg_pitch_score: 104,
        whiff_rate: 0.3,
      },
      {
        pitch_type: "FF",
        pitch_name: "4-Seam Fastball",
        count: 80,
        usage_pct: 0.8,
        avg_release_speed: 95,
        avg_release_spin_rate: 2300,
        avg_pfx_x_arm: 0.5,
        avg_pfx_z: 1.4,
        avg_pitch_score: 101,
        whiff_rate: 0.2,
      },
    ];

    render(<PitchTypeBars stats={stats} />);
    const rows = document.querySelectorAll(".bar-row");
    // The most-used pitch is the one a coach looks for first.
    expect(rows[0]?.textContent).toContain("FF");
    expect(rows[1]?.textContent).toContain("SL");
  });
});

// --- T9.6 loading, error and empty ----------------------------------------

describe("states", () => {
  it("tells loading, failed and empty apart", () => {
    const { rerender } = render(
      <Query data={undefined} isLoading error={null} what="pitches">
        {() => <div>rows</div>}
      </Query>,
    );
    expect(screen.getByText(/loading pitches/i)).toBeInTheDocument();

    rerender(
      <Query data={undefined} isLoading={false} error={new Error("boom")} what="pitches">
        {() => <div>rows</div>}
      </Query>,
    );
    // "Failed" and "empty" are different answers, and a dashboard that shows
    // the same blank panel for both leaves the viewer unable to tell whether
    // the data is missing or the request broke.
    expect(screen.getByText(/could not load pitches/i)).toBeInTheDocument();
    expect(screen.getByText("boom")).toBeInTheDocument();

    rerender(
      <Query
        data={[]}
        isLoading={false}
        error={null}
        what="pitches"
        isEmpty={(d) => d.length === 0}
        empty="No pitches recorded."
      >
        {() => <div>rows</div>}
      </Query>,
    );
    expect(screen.getByText("No pitches recorded.")).toBeInTheDocument();

    rerender(
      <Query data={["a"]} isLoading={false} error={null} what="pitches">
        {() => <div>rows</div>}
      </Query>,
    );
    expect(screen.getByText("rows")).toBeInTheDocument();
  });

  it("surfaces the request id so a browser failure can be found in the logs", () => {
    const error = new ApiError(
      500,
      { type: "x", title: "Internal", status: 500, request_id: "req-123" },
      "boom",
    );
    render(<ErrorState error={error} what="analytics" />);
    expect(screen.getByText(/req-123/)).toBeInTheDocument();
  });

  it("renders an empty message", () => {
    render(<Empty>Nothing here.</Empty>);
    expect(screen.getByText("Nothing here.")).toBeInTheDocument();
  });
});
