// Package check is the model shared by preflight and verification: results
// with levels, evidence, remediation and acceptance.
package check

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Levels.
const (
	OK      = "ok"
	Info    = "info"
	Warning = "warning"
	Blocker = "blocker"
)

// Scopes.
const (
	ScopeMigration = "migration"
	ScopeHost      = "host"
	ScopeSource    = "source"
	ScopeTarget    = "target"
	ScopeClusters  = "clusters"
	ScopeDatabase  = "database"
)

// Result is one check outcome for one scope.
type Result struct {
	ID           string         `json:"id"`
	CheckID      string         `json:"check_id"`
	Version      int            `json:"version"`
	Scope        string         `json:"scope"`
	Database     string         `json:"database,omitempty"`
	Level        string         `json:"level"`
	Hard         bool           `json:"hard"`
	Title        string         `json:"title"`
	Message      string         `json:"message"`
	Evidence     map[string]any `json:"evidence"`
	EvidenceHash string         `json:"evidence_hash"`
	Remediation  string         `json:"remediation"`
	DurationMS   int64          `json:"duration_ms"`
	Accepted     *Acceptance    `json:"accepted,omitempty"`
}

// Acceptance records an operator accepting an acceptable blocker.
type Acceptance struct {
	ID         string `json:"id"`
	Reason     string `json:"reason"`
	AcceptedBy string `json:"accepted_by"`
	AcceptedAt int64  `json:"accepted_at"`
}

// Definition describes a registered check.
type Definition struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Scope   string `json:"scope"`
	Title   string `json:"title"`
	FailsAs string `json:"fails_as"` // hard, acceptable, warning, info
}

// Hash returns a stable hash of the material evidence. Keys starting with
// "_" (timings, samples) are left out so re-runs with the same facts keep
// their acceptance.
func Hash(e map[string]any) string {
	keys := make([]string, 0, len(e))
	for k := range e {
		if !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		b, _ := json.Marshal(e[k])
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Finalize fills the evidence hash.
func (r *Result) Finalize() {
	if r.Evidence == nil {
		r.Evidence = map[string]any{}
	}
	r.EvidenceHash = Hash(r.Evidence)
}

// Blocking reports whether r blocks start: a hard blocker, or an acceptable
// blocker without a matching acceptance.
func (r Result) Blocking() bool {
	if r.Level != Blocker {
		return false
	}
	return r.Hard || r.Accepted == nil
}

// Summary counts results by outcome.
type Summary struct {
	Total       int  `json:"total"`
	OK          int  `json:"ok"`
	Info        int  `json:"info"`
	Warnings    int  `json:"warnings"`
	Blockers    int  `json:"blockers"`
	Hard        int  `json:"hard"`
	Accepted    int  `json:"accepted"`
	CanStart    bool `json:"can_start"`
	NeedsReview bool `json:"needs_warning_review"`
}

// Summarize computes the summary.
func Summarize(rs []Result) Summary {
	s := Summary{Total: len(rs)}
	for _, r := range rs {
		switch r.Level {
		case OK:
			s.OK++
		case Info:
			s.Info++
		case Warning:
			s.Warnings++
		case Blocker:
			if r.Hard {
				s.Hard++
			} else if r.Accepted != nil {
				s.Accepted++
			} else {
				s.Blockers++
			}
		}
	}
	s.CanStart = s.Hard == 0 && s.Blockers == 0
	s.NeedsReview = s.Warnings > 0
	return s
}
