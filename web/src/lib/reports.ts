export interface ReportDefinition {
  version: number;
  name: string;
  template: string;
  scope: 'self' | 'team' | 'organization';
  team_id?: string;
  start?: string;
  end?: string;
  period: 'last_7_days' | 'last_30_days' | 'previous_month' | 'custom';
  source_period?: 'last_7_days' | 'last_30_days' | 'previous_month' | 'custom';
  timezone: string;
  compare: boolean;
  group_mode: 'historical' | 'current';
  dimensions: string[];
  metrics: string[];
  filters: Record<string, string[]>;
  sections: string[];
  scenario_discount_percent: number;
}
export interface ReportColumn {
  key: string;
  label: string;
  unit: string;
}
export interface ReportRow {
  dimensions: Record<string, string>;
  values: Record<string, number | null>;
}
export interface ReportSection {
  id: string;
  title: string;
  columns: ReportColumn[];
  rows: ReportRow[];
  notes: string[];
}
export interface ReportResult {
  version: number;
  definition: ReportDefinition;
  generated_at: string;
  data_cutoff: string;
  start: string;
  end: string;
  columns: ReportColumn[];
  rows: ReportRow[];
  totals: Record<string, number | null>;
  comparison_reliable?: boolean;
  comparison_warnings?: string[];
  previous_totals?: Record<string, number | null>;
  warnings: string[];
  sections: ReportSection[];
  source_rows: number;
}
export interface ReportRun {
  id: string;
  report_id?: string;
  owner_user_id: string;
  status: 'queued' | 'running' | 'complete' | 'failed' | 'cancelled' | 'expired';
  definition: ReportDefinition;
  result?: ReportResult;
  error?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
  expires_at?: string;
  attempts: number;
  schedule_id?: string;
}
export interface SavedReport {
  id: string;
  owner_user_id: string;
  definition: ReportDefinition;
  shared: boolean;
  revision: number;
  created_at: string;
  updated_at: string;
}
export interface ReportSchedule {
  id: string;
  owner_user_id?: string;
  report_id: string;
  frequency: 'daily' | 'weekly' | 'monthly';
  timezone: string;
  at: string;
  weekday: number;
  monthday: number;
  enabled: boolean;
  next_run_at?: string;
  error?: string;
  last_error?: string;
}
export interface ReportBudget {
  id: string;
  owner_user_id?: string;
  name: string;
  scope: 'user' | 'team' | 'organization';
  subject_id: string;
  amount_nanousd: number;
  start: string;
  end: string;
}
export interface Catalog {
  version: number;
  dimensions: { id: string; label: string; unit: string; description: string }[];
  metrics: { id: string; label: string; unit: string; description: string }[];
  templates: ReportDefinition[];
}
export interface ReportOptions {
  dimensions: Record<string, { id: string; label: string }[]>;
}
export const activeRun = (run?: ReportRun) => run?.status === 'queued' || run?.status === 'running';
export const isCostMetric = (id: string, unit = '') => /cost|spend|saving|price|usd|discount/i.test(`${id} ${unit}`);
