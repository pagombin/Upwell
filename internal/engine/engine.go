// Package engine defines the contract every migration engine implements. The
// orchestrator, UI and store know only this interface.
package engine

import (
	"context"
	"time"

	"github.com/pagombin/upwell/internal/pg"
)

// Capabilities says what an engine can do.
type Capabilities struct {
	Online               bool     `json:"online"`
	Offline              bool     `json:"offline"`
	ResumeDuringCDC      bool     `json:"resume_during_cdc"`
	ResumeDuringBaseCopy bool     `json:"resume_during_base_copy"`
	GracefulStop         bool     `json:"graceful_stop"`
	TableSplitting       bool     `json:"table_splitting"`
	DecodingPlugins      []string `json:"decoding_plugins"`
	CopiesRoles          bool     `json:"copies_roles"`
	CopiesExtensions     bool     `json:"copies_extensions"`
}

// DatabaseSpec describes one database run.
type DatabaseSpec struct {
	MigrationID string
	Source      string // source database name
	Target      string // target database name
	SourceConn  pg.Conn
	TargetConn  pg.Conn
	Instance    string
	SlotName    string
	OriginName  string
	Plugin      string // empty: engine default
	RunDir      string
	TableJobs   int
	// TCPUserTimeout ends an engine connection whose data goes unacknowledged
	// this long (0: the kernel's own limit). D90, D98.
	TCPUserTimeout time.Duration
	IndexJobs      int
	SplitLarger    string
	// DropIfExists cleans the target of every object in the source's schema
	// before restoring it: set on a launch that follows an earlier attempt
	// (a restart from zero), whose objects may still be on the target.
	DropIfExists bool
	// Offline copies once without streaming: no slot, origin or plugin.
	Offline bool
	// SkipDBProperties: Upwell copies the database's ALTER DATABASE SET
	// properties itself (during a write freeze, D65).
	SkipDBProperties bool
	// ExcludeSchemas are left out of the copy entirely (a source provider's
	// management extension, D76). Written to the run directory's filters
	// file at the start, and reused from it on resume.
	ExcludeSchemas []string
	ExtraArgs      []string // test hook only; never set from the API
}

// RunPlan is the exact command for one database: what the dry run shows.
type RunPlan struct {
	Instance string            `json:"instance"`
	Unit     string            `json:"unit"`
	Binary   string            `json:"binary"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"-"`
	RunDir   string            `json:"run_dir"`
	WorkDir  string            `json:"work_dir"`
	LogFile  string            `json:"log_file"`
	PassFile string            `json:"-"`
	Resume   bool              `json:"resume"`
	Display  string            `json:"command"`
}

// Phase values reported by Observe.
const (
	PhaseUnknown  = "unknown"
	PhaseBaseCopy = "base_copy"
	PhaseCDC      = "cdc"
)

// Progress is a cheap snapshot.
type Progress struct {
	Phase        string `json:"phase"`
	ApplyEnabled bool   `json:"apply_enabled"`
	StartPos     string `json:"startpos"`
	EndPos       string `json:"endpos"`
	WriteLSN     string `json:"write_lsn"`
	FlushLSN     string `json:"flush_lsn"`
	ReplayLSN    string `json:"replay_lsn"`
}

// LogRecord is one parsed engine log line.
type LogRecord struct {
	TS    time.Time
	PID   int
	Level string
	Step  string
	Table string
	Msg   string
}

// Failure classes.
const (
	FailNone          = ""
	FailTransient     = "transient"
	FailPermanent     = "permanent"
	FailSlotLost      = "slot_lost"
	FailChangesLost   = "changes_lost" // the changes the target still needs are gone (D108)
	FailBaseCopy      = "base_copy_failed"
	FailEndposReached = "endpos_reached" // not a failure: a clean end
)

// Engine is the contract.
type Engine interface {
	Name() string
	Version(ctx context.Context) (string, error)
	Capabilities() Capabilities
	Plan(d DatabaseSpec, resume bool) (RunPlan, error)
	Start(ctx context.Context, p RunPlan, d DatabaseSpec) error
	Observe(ctx context.Context, p RunPlan, d DatabaseSpec) (Progress, error)
	SetEndPosition(ctx context.Context, p RunPlan, d DatabaseSpec) (string, error)
	Nudge(ctx context.Context, d DatabaseSpec) error
	Stop(ctx context.Context, p RunPlan, d DatabaseSpec) (StopResult, error)
	// ReleaseSlot waits for the database's replication slot to be inactive,
	// terminating a stale walsender after 15 s.
	ReleaseSlot(ctx context.Context, d DatabaseSpec) (released, terminated bool, err error)
	Cleanup(ctx context.Context, d DatabaseSpec, opts CleanupOptions) ([]string, error)
	ParseLog(line string) LogRecord
	Classify(r LogRecord) string
	CopyRoles(ctx context.Context, src, dst pg.Conn, runDir string) (string, error)
}

// Retargeter is implemented by an engine that records its endpoints and
// must be told when a stopped database moves to another address of the same
// clusters (D100). It reports whether anything changed.
type Retargeter interface {
	Retarget(ctx context.Context, d DatabaseSpec) (bool, error)
}

// StopResult reports how a stop went.
type StopResult struct {
	Duration          time.Duration `json:"duration_ms"`
	SlotReleased      bool          `json:"slot_released"`
	WalsenderKilled   bool          `json:"walsender_terminated"`
	SlotReleaseWaited time.Duration `json:"slot_wait_ms"`
}

// CleanupOptions selects what cleanup removes.
type CleanupOptions struct {
	DropHeartbeat bool
	RemoveWorkDir bool
}
