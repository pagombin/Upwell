package pgpack

import (
	"github.com/jackc/pgx/v5"

	"fmt"
	"strings"
)

// identityDetail is one table in the replica_identity evidence (E4).
type identityDetail struct {
	Table      string `json:"table"`
	Owner      string `json:"owner"`
	OwnedByYou bool   `json:"owned_by_you"`
	Identity   string `json:"identity"`
	Updates    int64  `json:"updates"`
	Deletes    int64  `json:"deletes"`
	SizeBytes  int64  `json:"size_bytes"`
	Cost       string `json:"full_cost"`
}

// largeTableBytes is where adding a primary key (a full rewrite under an
// ACCESS EXCLUSIVE lock) stops being a reasonable suggestion.
const largeTableBytes = 1 << 30

// fullCost says plainly what REPLICA IDENTITY FULL costs for one table.
func fullCost(d identityDetail) string {
	if d.Updates == 0 && d.Deletes == 0 {
		return "cheap: no updates or deletes recorded since statistics were last reset, and FULL costs nothing for inserts"
	}
	s := fmt.Sprintf("costly: %d update(s) and %d delete(s) recorded since statistics were last reset; with FULL each one logs the whole old row, and on the target each replayed one compares every column, a scan of the table per row unless the target table has an index the apply can use", d.Updates, d.Deletes)
	if d.SizeBytes >= largeTableBytes {
		s += fmt.Sprintf("; adding a primary key would rewrite this %s table under an exclusive lock, so prefer FULL and an index on the target", humanBytes(float64(d.SizeBytes)))
	} else {
		s += "; a primary key avoids this cost"
	}
	return s
}

// replicaIdentityAdvice builds the replica_identity blocker: the failure as
// it happens under the database's decoding plugin (preflight finding 1),
// who can fix it (5), what each fix costs (2, 4) and copy-ready SQL with a
// lock timeout (3, E3). plugin is "" when the plugin choice is unknown.
func replicaIdentityAdvice(plugin, user string, count int, details []identityDetail) (msg, fix string, ev map[string]any) {
	example := ""
	if len(details) > 0 {
		example = details[0].Table
	}
	msg = fmt.Sprintf("%d table(s) have no usable replica identity: no primary key, REPLICA IDENTITY NOTHING, or an identity index that was dropped (for example %s).", count, example)
	switch plugin {
	case "pgoutput":
		msg += " With pgoutput the migration's publication includes them, so while the migration runs the application's UPDATE and DELETE on them would fail with an error on the source."
	case "":
		msg += " Under pgoutput the application's UPDATE and DELETE on them would fail with an error on the source; under test_decoding they would succeed on the source but cannot be applied on the target."
	default:
		msg += fmt.Sprintf(" This database uses %s, which needs no publication: the application's UPDATE and DELETE on them succeed on the source but are decoded without the old row's key, so they cannot be applied on the target, which would fall behind or differ from the source.", plugin)
	}
	var others []string
	for i := range details {
		details[i].Cost = fullCost(details[i])
		if !details[i].OwnedByYou {
			others = append(others, details[i].Table+" (owner "+details[i].Owner+")")
		}
	}
	if len(others) > 0 {
		msg += fmt.Sprintf(" %d of them are owned by another role, for example %s: ALTER TABLE requires ownership and %s is not a superuser, so their owner (on a customer cluster, the customer) must run the fix.", len(others), others[0], user)
	} else if len(details) > 0 {
		msg += fmt.Sprintf(" %s owns them and can run the fix.", user)
	}
	fix = "Run the fix as each table's owner (ALTER TABLE ... REPLICA IDENTITY requires ownership), then run preflight again. " +
		"For most tables use REPLICA IDENTITY FULL, with a lock timeout: SET lock_timeout = '5s'; ALTER TABLE <table> REPLICA IDENTITY FULL; " +
		"the ALTER needs an ACCESS EXCLUSIVE lock only briefly, but if a transaction holds the table it would wait and every later query on the table would queue behind it; the timeout stops that, so if it times out, retry later. " +
		"FULL logs the whole old row on every UPDATE and DELETE, and on the target each replayed UPDATE or DELETE compares every column, which is a scan per row when the target table has no usable index: cheap for insert-only tables, costly for tables with many updates or deletes (each table's updates, deletes and cost are in the evidence). " +
		"A primary key avoids that cost, but when the table has no natural key, adding one means adding an identity column, which rewrites the whole table under an ACCESS EXCLUSIVE lock; prefer FULL for large tables. " +
		"Copy-ready SQL with every table name quoted is in the evidence (fix_sql)."
	var b strings.Builder
	b.WriteString("-- Run as each table's owner, then run preflight again.\n-- If a statement times out, a transaction is holding the table: retry later.\nSET lock_timeout = '5s';\n")
	for _, d := range details {
		fmt.Fprintf(&b, "ALTER TABLE %s REPLICA IDENTITY FULL;\n", d.Table)
	}
	if count > len(details) {
		fmt.Fprintf(&b, "-- %d more table(s) not listed; run preflight again after these.\n", count-len(details))
	}
	ev = map[string]any{"plugin": plugin, "details": details, "fix_sql": b.String()}
	return msg, fix, ev
}

// EvidenceFor is the evidence with the grant and revoke scripts for user.
func (a ApplyPrivileges) EvidenceFor(user string, verNum int) map[string]any {
	ev := a.Evidence()
	ev["grant_script"], ev["revoke_script"] = ApplyPrivilegeScripts(user, verNum)
	return ev
}

// ApplyPrivilegeScripts returns the SQL the platform runs on the target, as
// a superuser, to give user what pgcopydb's apply needs, and the SQL that
// takes it back after cutover (preflight findings 6 and 8, E3).
func ApplyPrivilegeScripts(user string, verNum int) (grant, revoke string) {
	u := pgx.Identifier{user}.Sanitize()
	var fns []string
	for _, f := range originFuncs {
		fns = append(fns, "pg_catalog."+f)
	}
	list := strings.Join(fns, ",\n  ")
	head := "-- Run on the target as a superuser (the platform).\n" +
		"-- Function grants are per database: run this in every target database that\n" +
		"-- already exists, and in template1 so databases created from it inherit them.\n" +
		"-- A database created from template0 (when the source's encoding or locale\n" +
		"-- differs from template1's) does not inherit them: run this in it as well.\n"
	grant = head + "GRANT EXECUTE ON FUNCTION\n  " + list + "\nTO " + u + ";\n"
	revoke = "-- After cutover: take back what the migration needed. Run in the same\n-- databases as the grant script, including template1.\nREVOKE EXECUTE ON FUNCTION\n  " + list + "\nFROM " + u + ";\n"
	if verNum >= 150000 {
		grant += "-- Cluster-wide, once:\nGRANT SET ON PARAMETER session_replication_role TO " + u + ";\n"
		revoke += "-- Cluster-wide, once:\nREVOKE SET ON PARAMETER session_replication_role FROM " + u + ";\n"
	} else {
		grant += "-- PostgreSQL before 15 cannot grant SET on session_replication_role:\n-- only a superuser can set it. Upgrade the target to PostgreSQL 15 or later.\n"
	}
	return grant, revoke
}

// ApplyPrivilegeRemediation explains who runs the grants, where, what they
// allow, and how to take them back.
func ApplyPrivilegeRemediation(user string, verNum int) string {
	s := "Ask the platform to run the grant script from the evidence on the target as a superuser (" + user + " cannot grant these to itself), then run preflight again. "
	if verNum > 0 && verNum < 150000 {
		s = "Before PostgreSQL 15, only a superuser can set session_replication_role, so upgrade the target to PostgreSQL 15 or later; the origin function grants are in the grant script (run by the platform as a superuser). "
	}
	return s + "Run the function grants in each target database and in template1. Databases Upwell creates from template0 (when the source's encoding or locale differs from template1's) do not inherit them, so the launch will stop and ask for the grants in that database. " +
		"Security trade-off: SET on session_replication_role lets " + user + " disable ordinary triggers and foreign-key enforcement in its own sessions (foreign keys are enforced by triggers), and the origin functions let it create, advance and drop any replication origin, including the pg_<oid> origins native logical-replication subscriptions use. " +
		"Take them back after cutover with the revoke script from the evidence (the report's post-cutover checklist lists it)."
}
