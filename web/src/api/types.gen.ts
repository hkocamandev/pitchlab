// Code generated from the Go response types. DO NOT EDIT.
//
// Regenerate with: make ts-types
//
// These mirror the structs in internal/api/dto.go. They are generated rather
// than written by hand so that a field renamed on the server cannot compile
// cleanly on both sides and fail in a browser.

export interface Athlete {
  id: string;
  mlbam_id: number;
  full_name: string;
  throws: string | null;
  bats: string | null;
  primary_role: string;
  birth_date?: string | null;
}

export interface AthleteTotals {
  session_count: number;
  pitch_count: number;
  first_pitch_at?: string | null;
  last_pitch_at?: string | null;
}

export interface AthleteListItem {
  id: string;
  mlbam_id: number;
  full_name: string;
  throws: string | null;
  bats: string | null;
  primary_role: string;
  birth_date?: string | null;
  session_count: number;
  pitch_count: number;
}

export interface AthleteDetail {
  id: string;
  mlbam_id: number;
  full_name: string;
  throws: string | null;
  bats: string | null;
  primary_role: string;
  birth_date?: string | null;
  totals: AthleteTotals;
}

export interface SessionRef {
  id: string;
  full_name: string;
  throws?: string | null;
}

export interface SessionMetrics {
  pitch_count: number;
  avg_release_speed: number | null;
  avg_release_spin_rate: number | null;
  avg_pitch_score: number | null;
  outcome_distribution?: Record<string, number>;
}

export interface Session {
  id: string;
  external_session_uid: string;
  pitcher_athlete_id: string;
  external_game_ref?: string | null;
  source_game_date?: string | null;
  opponent_team?: string | null;
  started_at: string;
  ended_at?: string | null;
  status: string;
  is_synthetic: boolean;
  metrics: SessionMetrics;
}

export interface SessionDetail {
  id: string;
  external_session_uid: string;
  pitcher_athlete_id: string;
  external_game_ref?: string | null;
  source_game_date?: string | null;
  opponent_team?: string | null;
  started_at: string;
  ended_at?: string | null;
  status: string;
  is_synthetic: boolean;
  metrics: SessionMetrics;
  pitcher?: SessionRef | null;
  anomaly_count: number;
}

export interface PitchContext {
  balls: number;
  strikes: number;
  outs_when_up: number;
  inning: number;
  inning_topbot?: string | null;
  stand: string;
  p_throws: string;
  on_1b: boolean;
  on_2b: boolean;
  on_3b: boolean;
}

export interface Measurement {
  release_speed: number | null;
  release_spin_rate: number | null;
  spin_axis: number | null;
  pfx_x: number | null;
  pfx_z: number | null;
  plate_x: number | null;
  plate_z: number | null;
  release_extension: number | null;
  release_pos_x: number | null;
  release_pos_z: number | null;
  arm_angle: number | null;
  pfx_x_arm: number | null;
  plate_x_bat: number | null;
  zone_height_norm: number | null;
  in_zone: boolean | null;
  measurement_quality: string;
}

export interface Prediction {
  model_version: string;
  variant: string;
  target: string;
  probabilities?: Record<string, number>;
  predicted_class?: string | null;
  whiff_probability?: number | null;
  scored_quantity: number;
  score: number;
}

export interface Pitch {
  id: string;
  session_id: string;
  session_pitch_index: number;
  at_bat_number: number;
  pitch_number: number;
  thrown_at: string;
  context: PitchContext;
  pitch_type: string | null;
  pitch_name: string | null;
  measurement?: Measurement | null;
  predictions?: Prediction[];
  actual_outcome: string | null;
  prediction_status: string;
  correlation_id?: string | null;
}

export interface Anomaly {
  id: string;
  athlete_id: string;
  session_id: string;
  metric: string;
  pitch_type?: string | null;
  observed_value: number;
  baseline_median: number;
  robust_sigma: number;
  z_robust: number;
  direction: string;
  severity: string;
  window_sessions: number;
  detector_version: string;
  detected_at: string;
  acknowledged_at: string | null;
}

export interface ModelPeriod {
  start?: string | null;
  end?: string | null;
}

export interface ModelVersion {
  id: string;
  version: string;
  variant: string;
  target: string;
  algorithm: string;
  is_active: boolean;
  feature_count: number;
  class_labels?: string[];
  train_period: ModelPeriod;
  val_period: ModelPeriod;
  test_period: ModelPeriod;
  data_snapshot: string;
  git_sha?: string | null;
  metrics?: unknown;
  created_at: string;
}

export interface AthleteSummary {
  pitch_count: number;
  session_count: number;
  avg_release_speed: number | null;
  avg_release_spin_rate: number | null;
  avg_release_extension: number | null;
  avg_pitch_score: number | null;
  whiff_rate: number | null;
  called_strike_rate: number | null;
  in_zone_rate: number | null;
}

export interface PitchTypeStat {
  pitch_type: string;
  pitch_name?: string | null;
  count: number;
  usage_pct: number;
  avg_release_speed: number | null;
  avg_release_spin_rate: number | null;
  avg_pfx_x_arm: number | null;
  avg_pfx_z: number | null;
  avg_pitch_score: number | null;
  whiff_rate: number | null;
}

export interface TrendPoint {
  session_id: string;
  source_game_date?: string | null;
  started_at: string;
  pitch_count: number;
  avg_release_speed: number | null;
  avg_release_spin_rate: number | null;
  avg_pitch_score: number | null;
}

export interface Analytics {
  athlete_id: string;
  period: ModelPeriod;
  summary: AthleteSummary;
  pitch_type_distribution?: PitchTypeStat[];
  trend?: TrendPoint[];
  open_anomaly_count: number;
}

export interface FeatureContribution {
  feature: string;
  value: number | null;
  contribution: number;
}

export interface Explanation {
  pitch_id: string;
  variant: string;
  model_version: string;
  target: string;
  predicted_class?: string;
  probabilities?: Record<string, number>;
  target_class: string;
  space: string;
  base_value: number;
  predicted_value: number;
  predicted_probability: number;
  other_contribution: number;
  contributions: FeatureContribution[];
  inference_ms: number;
}

export interface LivePitchContext {
  balls: number;
  strikes: number;
  outs_when_up: number;
  inning: number;
  inning_topbot?: string;
  on_1b: boolean;
  on_2b: boolean;
  on_3b: boolean;
  bat_score?: number | null;
  fld_score?: number | null;
  n_thruorder_pitcher?: number | null;
}

export interface LiveMeasurement {
  pitch_type?: string;
  pitch_name?: string;
  release_speed?: number | null;
  release_spin_rate?: number | null;
  spin_axis?: number | null;
  release_pos_x?: number | null;
  release_pos_y?: number | null;
  release_pos_z?: number | null;
  release_extension?: number | null;
  pfx_x?: number | null;
  pfx_z?: number | null;
  plate_x?: number | null;
  plate_z?: number | null;
  sz_top?: number | null;
  sz_bot?: number | null;
  effective_speed?: number | null;
  arm_angle?: number | null;
  pfx_x_arm?: number | null;
  release_pos_x_arm?: number | null;
  plate_x_bat?: number | null;
  zone_height_norm?: number | null;
  in_zone?: boolean | null;
  normalization_version: number;
  measurement_quality: string;
}

export interface LivePrediction {
  variant: string;
  model_version: string;
  target: string;
  probabilities?: Record<string, number>;
  predicted_class?: string;
  whiff_probability?: number | null;
  scored_quantity: number;
  score: number;
}

export interface LiveAnomaly {
  anomaly_id: string;
  athlete_id: string;
  session_id: string;
  metric: string;
  pitch_type?: string;
  observed_value: number;
  baseline_median: number;
  robust_sigma: number;
  z_robust: number;
  direction: string;
  severity: string;
  window_sessions: number;
  detector_version: string;
  detected_at: string;
}

export interface LivePitch {
  pitch_id: string;
  session_pitch_index: number;
  at_bat_number: number;
  pitch_number: number;
  thrown_at: string;
  context: LivePitchContext;
  stand: string;
  p_throws: string;
  pitch_type?: string;
  pitch_name?: string;
  measurement: LiveMeasurement;
  predictions?: LivePrediction[];
  actual_outcome?: string;
  prediction_status: string;
  is_synthetic: boolean;
}

export interface LiveMetrics {
  pitch_count: number;
  last_pitch_at: string;
  last_release_speed?: number | null;
}

export interface Page {
  limit: number;
  next_cursor?: string;
  has_more: boolean;
}

export interface WSMessage {
  type: string;
  seq: number;
  ts: string;
  session_id: string;
  correlation_id?: string;
  data: unknown;
}

export interface WSError {
  code: string;
  message: string;
}

export interface WSClosed {
  reason: string;
  final_metrics?: unknown;
  dropped_total?: number;
}

