export type Role = "viewer" | "operator" | "admin";
export interface User { id: string; username: string; role: Role; disabled: boolean; last_login_at?: number; created_at: number }
export interface Flags {
  started?: boolean; paused?: boolean; aborted?: boolean; cleaned_up?: boolean; apps_switched?: boolean;
  preflight_running?: boolean; preflight_ok?: boolean; preflight_run_id?: string; verdict?: string; verdict_at?: number;
  verifying?: boolean; cleanup_running?: boolean; verify_run_id?: string;
  cutover?: { op_id: string; phase: string; started_at: number; writes_stopped_at?: number; ended_at?: number; write_pause_ms?: number; override?: boolean; verdict?: string };
}
export interface Migration {
  id: string; short_id: string; name: string; state: string; mode: string; engine: string;
  source_conn_id?: string; target_conn_id?: string; settings: Record<string, unknown>; settings_snapshot?: Record<string, unknown>;
  flags: Flags; fixture?: boolean; created_by?: string; created_at: number; updated_at: number; started_at?: number;
}
export interface Database {
  id: string; migration_id: string; source_name: string; target_name: string; include: boolean; skip_reason?: string; state: string;
  slot_name: string; origin_name: string; plugin?: string; instance: string; size_bytes: number; rows_estimate: number; table_count: number;
  last_error?: string; error_class?: string; in_sync_since?: number; backlog_bytes?: number; replay_lsn?: string; write_lsn?: string;
  hb_before_lsn?: string; hb_token?: string; endpos?: string; origin_lsn?: string; verdict?: string; retry_count: number; next_retry_at?: number;
  base_copy_done: boolean; created_at: number; updated_at: number;
}
export interface Connection { id: string; kind: string; host: string; port: number; user: string; dbname: string; sslmode: string; has_password: boolean; storage_gb?: number }
export interface Permission { customer: string; account_id: string; ticket: string; granted_by: string; granted_at: string; scope: string; notes?: string; locked_at?: number }
export interface MigrationView {
  migration: Migration; databases: Database[]; included: number; state_counts: Record<string, number>; total_bytes: number;
  permission?: Permission | null; source?: Connection; target?: Connection; settings_effective?: Record<string, unknown>;
  preflight?: PreflightRun | null; live?: Record<string, Record<string, number>>; cluster?: Record<string, number>; now?: number;
}
export interface CheckResult {
  id: string; check_id: string; version: number; scope: string; database?: string; level: "ok" | "info" | "warning" | "blocker"; hard: boolean;
  title: string; message: string; evidence: Record<string, unknown>; evidence_hash: string; remediation: string; duration_ms: number;
  accepted?: { id: string; reason: string; accepted_by: string; accepted_at: number };
}
export interface Summary { total: number; ok: number; info: number; warnings: number; blockers: number; hard: number; accepted: number; can_start: boolean; needs_warning_review: boolean }
export interface PreflightRun { id: string; migration_id: string; operation_id: string; state: string; started_at: number; ended_at?: number; summary: Summary; fingerprint: string }
export interface Step { key: string; title: string; state: "waiting" | "running" | "done" | "failed" | "skipped"; started_at?: number; ended_at?: number; detail?: string; progress?: number }
export interface Operation { id: string; migration_id?: string; kind: string; state: string; steps: Step[]; params: Record<string, unknown>; started_by?: string; started_at: number; ended_at?: number; result?: Record<string, unknown>; error?: string }
export interface LogRecord { seq?: number; ts: number; level: string; component: string; migration?: string; database?: string; attempt?: number; step?: string; table?: string; op?: string; msg: string; raw?: string }
export interface EventRow { id: string; seq: number; migration_id: string; database: string; type: string; severity: string; message: string; data: unknown; ts: number }
export interface Alert { id: string; migration_id?: string; rule: string; scope: string; severity: string; state: string; message: string; first_at: number; last_at: number; acked_by?: string; ack_note?: string; acked_at?: number; resolved_at?: number }
export interface SettingItem { key: string; type: string; default: unknown; min?: number; max?: number; enum?: string[]; scope: string; group: string; description: string; unit?: string; value: unknown; updated_by?: string; updated_at?: number }
export interface Point { ts: number; v: number; min?: number; max?: number }
export interface Series { name: string; database?: string; points: Point[] }
export interface VerificationRow { database: string; check_id: string; result: string; title: string; detail: { message?: string; evidence?: Record<string, unknown> }; ts: number }
export interface AuditEntry { seq: number; id: string; ts: number; user_id?: string; username?: string; ip?: string; action: string; target?: string; before?: unknown; after?: unknown; prev_hash: string; hash: string }
