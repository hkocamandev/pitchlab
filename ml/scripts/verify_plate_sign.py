"""Empirically verify the plate_x sign convention.

`plate_x` is documented as "from the catcher's perspective" but the direction
of the positive axis is easy to reason about backwards, and getting it wrong
silently mirrors every location feature.

Rather than argue from the diamond geometry, this derives it from the data:

  * Hit-by-pitch events must cluster on the side of the plate where the batter
    is standing, because the ball hit the batter.
  * The Statcast `zone` grid (1-9, read left-to-right from the catcher's view)
    gives a second, independent read.

Usage:
    .venv/bin/python ml/scripts/verify_plate_sign.py
"""

from __future__ import annotations

import sys
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import config


def latest_snapshot() -> Path:
    return sorted(config.RAW_DIR.glob("statcast_*.parquet"))[-1]


def main() -> int:
    cols = ["plate_x", "stand", "description", "zone", "p_throws", "pfx_x"]
    df = pd.read_parquet(latest_snapshot(), columns=cols)

    print("=" * 68)
    print("EVIDENCE 1 -- hit-by-pitch location by batter stance")
    print("=" * 68)
    hbp = df[df["description"].eq("hit_by_pitch") & df["plate_x"].notna()]
    print(f"{len(hbp):,} hit-by-pitch rows with a plate_x reading\n")
    summary = hbp.groupby("stand")["plate_x"].agg(["size", "mean", "median"])
    print(summary.to_string())
    print()

    rhb = summary.loc["R", "mean"] if "R" in summary.index else float("nan")
    lhb = summary.loc["L", "mean"] if "L" in summary.index else float("nan")
    print(f"  right-handed batters get hit at mean plate_x = {rhb:+.3f}")
    print(f"  left-handed  batters get hit at mean plate_x = {lhb:+.3f}")
    print()

    if rhb < 0 < lhb:
        verdict = "NEGATIVE plate_x is the right-handed batter's side"
        rhb_sign = -1
    elif lhb < 0 < rhb:
        verdict = "POSITIVE plate_x is the right-handed batter's side"
        rhb_sign = +1
    else:
        print("  INCONCLUSIVE -- the two stances did not separate")
        return 1
    print(f"  => {verdict}")
    print()

    print("=" * 68)
    print("EVIDENCE 2 -- Statcast `zone` grid")
    print("=" * 68)
    print("Zones 1/4/7 are one edge of the grid, 3/6/9 the other.\n")
    z = df[df["zone"].isin([1, 4, 7, 3, 6, 9]) & df["plate_x"].notna()]
    edge = z.assign(
        edge=z["zone"].isin([1, 4, 7]).map({True: "zones 1/4/7", False: "zones 3/6/9"})
    )
    print(edge.groupby("edge")["plate_x"].agg(["size", "mean"]).to_string())
    print()

    print("=" * 68)
    print("CONCLUSION")
    print("=" * 68)
    if rhb_sign == +1:
        print("  plate_x_bat = plate_x        for RHB")
        print("  plate_x_bat = -plate_x       for LHB")
    else:
        print("  plate_x_bat = -plate_x       for RHB")
        print("  plate_x_bat = plate_x        for LHB")
    print("  ...so that POSITIVE plate_x_bat means INSIDE to the batter.\n")

    current = "features.py maps stand=='R' -> plate_x unchanged"
    expected_ok = rhb_sign == +1
    print(f"  Current implementation: {current}")
    print(f"  Status: {'CORRECT' if expected_ok else 'INVERTED -- features.py must flip'}")
    print()

    print("=" * 68)
    print("CROSS-CHECK -- pfx_x arm-side mirroring")
    print("=" * 68)
    ff = df[df["pfx_x"].notna()]
    print(ff.groupby("p_throws")["pfx_x"].agg(["size", "mean"]).to_string())
    print()
    print("  Both handedness groups should show arm-side run, i.e. means of")
    print("  opposite sign. After mirroring in features.py they must agree.")
    return 0 if expected_ok else 2


if __name__ == "__main__":
    raise SystemExit(main())
