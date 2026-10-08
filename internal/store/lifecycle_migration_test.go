package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestMigrationFenceRejectsNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at)
		VALUES(31, '031_future', 'abc123', '2027-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "database records schema migration 31, which this binary does not know; refusing to open a database written by a newer or incompatible shephrd") {
		t.Fatalf("newer database opened, err = %v", err)
	}
}

func TestMigrationFenceRejectsIdentityMismatch(t *testing.T) {
	for name, corrupt := range map[string]string{
		"Go migration checksum mismatch": `UPDATE schema_migrations SET checksum='0000000000000000000000000000000000000000000000000000000000000000' WHERE version=9`,
		"bridge checksum mismatch":       `UPDATE schema_migrations SET checksum='0000000000000000000000000000000000000000000000000000000000000000' WHERE version=14`,
		"bridge name mismatch":           `UPDATE schema_migrations SET name='014_something_else' WHERE version=14`,
		"bridge missing identity":        `UPDATE schema_migrations SET name='', checksum='' WHERE version=13`,
		"historical wrong checksum":      `UPDATE schema_migrations SET checksum='0000000000000000000000000000000000000000000000000000000000000000' WHERE version=3`,
		"noncontiguous history":          `DELETE FROM schema_migrations WHERE version=14`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db := buildLegacyDBAt(t, path, 15)
			if _, err := db.Exec(corrupt); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("incompatibly recorded migration identity was accepted")
			}
		})
	}
	for name, corrupt := range map[string]string{
		"baseline name mismatch":  `UPDATE schema_migrations SET name='027_plan_surface' WHERE version=` + strconv.Itoa(baselineVersion),
		"baseline checksum drift": `UPDATE schema_migrations SET checksum='0000000000000000000000000000000000000000000000000000000000000000' WHERE version=` + strconv.Itoa(baselineVersion),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			state, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := state.db.Exec(corrupt); err != nil {
				t.Fatal(err)
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("incompatibly recorded baseline identity was accepted")
			}
		})
	}
}

func TestShapeFenceRejectsMaskedIncompatibleSchema(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*sql.DB) error
		want   string
	}{
		{"missing critical column", func(db *sql.DB) error {
			_, err := db.Exec(`ALTER TABLE plan_items DROP COLUMN description`)
			return err
		}, "plan_items"},
		{"missing critical trigger", func(db *sql.DB) error {
			_, err := db.Exec(`DROP TRIGGER plan_items_dispatched_scope_immutable`)
			return err
		}, "plan_items_dispatched_scope_immutable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			state, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.mutate(state.db); err != nil {
				t.Fatal(err)
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("masked incompatible schema opened, err = %v", err)
			}
		})
	}
}

func TestShapeFenceRejectsReintroducedRetiredObjects(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*sql.DB) error
		want   string
	}{
		{"reintroduced landing duplicate column", func(db *sql.DB) error {
			_, err := db.Exec(`ALTER TABLE tasks ADD COLUMN landed INTEGER NOT NULL DEFAULT 0`)
			return err
		}, "table tasks"},
		{"reintroduced retired table", func(db *sql.DB) error {
			_, err := db.Exec(`CREATE TABLE memory (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, content TEXT NOT NULL)`)
			return err
		}, "memory"},
		{"reintroduced live task-list table", func(db *sql.DB) error {
			_, err := db.Exec(`CREATE TABLE task_lists (id TEXT PRIMARY KEY, name TEXT NOT NULL, driver_id TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`)
			return err
		}, "task_lists"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			state, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.mutate(state.db); err != nil {
				t.Fatal(err)
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("reintroduced retired object accepted, err = %v", err)
			}
		})
	}
}

func TestAttemptLifecycleInvariants(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: "/repo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "feature", Objective: "work"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, violation := range map[string]string{
		"released_at without state":    `UPDATE attempts SET released_at='2026-08-14T00:00:00Z' WHERE id='ATTEMPT'`,
		"released state without time":  `UPDATE attempts SET release_state='released' WHERE id='ATTEMPT'`,
		"landed_proven without proof":  `UPDATE attempts SET landed_proven=1 WHERE id='ATTEMPT'`,
		"landed_proven without stamp":  `UPDATE attempts SET landed_proven=1, landing_kind='local_default_branch' WHERE id='ATTEMPT'`,
		"invalid release state string": `UPDATE attempts SET release_state='vanished' WHERE id='ATTEMPT'`,
	} {
		if _, err := state.db.Exec(strings.ReplaceAll(violation, "ATTEMPT", attempt.ID)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := state.db.Exec(`UPDATE attempts SET released_at='2026-08-14T00:00:00Z', release_state='released' WHERE id=?`, attempt.ID); err != nil {
		t.Fatalf("coherent release write rejected: %v", err)
	}
}

func TestCompleteLandingProofValidation(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: "/repo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "feature", Objective: "work"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.CompleteLandingProof(attempt.ID); err == nil {
		t.Fatal("missing proof validated")
	}
	if _, err := state.db.Exec(`UPDATE attempts SET landed_proven=1, landing_kind='report_artifact', landed_verified_at='2026-08-14T00:00:00Z' WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CompleteLandingProof(attempt.ID); err == nil || !strings.Contains(err.Error(), "verified report artifact") {
		t.Fatalf("report proof without immutable artifact validated, err = %v", err)
	}
	if _, err := state.db.Exec(`UPDATE attempts SET landing_kind='local_default_branch' WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CompleteLandingProof(attempt.ID); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("code proof without identity validated, err = %v", err)
	}
	if _, err := state.db.Exec(`UPDATE attempts SET landed_source_commit='aaa', landed_target_ref='refs/heads/main', landed_target_commit='bbb', landed_checkpoint_revision=2 WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	proof, err := state.CompleteLandingProof(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Kind != "local_default_branch" || proof.SourceCommit != "aaa" || proof.TargetCommit != "bbb" {
		t.Fatalf("validated proof = %+v", proof)
	}
}

func TestMarkNoWorkspace(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: "/repo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "feature", Objective: "work"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.MarkNoWorkspace(attempt.ID, "acquire failed before any worktree existed"); err != nil {
		t.Fatal(err)
	}
	current, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ReleaseState != "no_workspace" || current.ReleasedAt != nil || current.ReleaseReason == "" {
		t.Fatalf("no-workspace attempt = %+v", current)
	}
	claimed, claimState, err := state.ClaimRelease(attempt.ID)
	if err != nil || claimed || claimState != "no_workspace" {
		t.Fatalf("no-workspace release claim = %v %q, err = %v", claimed, claimState, err)
	}
	if err := state.MarkNoWorkspace(attempt.ID, "again"); err == nil {
		t.Fatal("no-workspace classification applied twice")
	}
	other, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "other", Objective: "work"})
	if err != nil {
		t.Fatal(err)
	}
	configured, err := state.BeginAttempt(other.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(configured.ID, "session", "/tree", "lease-1", "branch"); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkNoWorkspace(configured.ID, "must not apply"); err == nil {
		t.Fatal("attempt with a recorded workspace was classified as no_workspace")
	}
}

// TestReleasedMigrationChecksumsArePinned freezes the identity manifest. The
// executable bridge bodies must keep the checksums released databases carry,
// the frozen pre-floor fixture bodies must keep the checksums the released
// binaries recorded, and the baseline body's checksum is pinned so an edit to
// the consolidated DDL cannot silently change the baseline identity.
func TestReleasedMigrationChecksumsArePinned(t *testing.T) {
	sum := func(body string) string {
		digest := sha256.Sum256([]byte(body))
		return hex.EncodeToString(digest[:])
	}

	registry, err := loadBridgeMigrations()
	if err != nil {
		t.Fatal(err)
	}
	byVersion := make(map[int]bridgeMigration, len(registry))
	for _, entry := range registry {
		byVersion[entry.version] = entry
	}
	for version := 16; version <= 26; version++ {
		frozen := frozenMigrationIdentities[version]
		if got, want := byVersion[version].name, frozen.name; got != want {
			t.Fatalf("bridge migration %d name drifted: %q, want %q", version, got, want)
		}
		if got, want := byVersion[version].checksum(), frozen.checksum; got != want {
			t.Fatalf("bridge migration %d checksum drifted to %s; released migrations are immutable", version, got)
		}
	}
	if got, want := byVersion[21].checksum(), sum(migration21SQL); got != want {
		t.Fatalf("bridge migration 21 checksum drifted to %s", got)
	}

	frozenBodies := map[int]string{
		9:  migration9Identity,
		15: migration15Identity,
		21: migration21SQL,
		27: migration27SQL,
	}
	for _, source := range []struct {
		name   string
		system fs.FS
	}{
		{"testdata/legacy-migrations", legacyFixtureSQL},
		{"migrations", migrations},
	} {
		entries, err := fs.ReadDir(source.system, source.name)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
			if err != nil {
				t.Fatal(err)
			}
			body, err := fs.ReadFile(source.system, source.name+"/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			frozenBodies[version] = string(body)
		}
	}
	for version := 1; version <= 27; version++ {
		body, ok := frozenBodies[version]
		if !ok {
			t.Fatalf("no frozen body recorded for migration %d", version)
		}
		frozen := frozenMigrationIdentities[version]
		if got, want := sum(body), frozen.checksum; got != want {
			t.Fatalf("frozen migration %d identity drifted to %s; released databases would be stranded", version, got)
		}
	}
	if got := sum(baselineBody); got != "520007ec7246b9b5cb1b4863e3d8ba3da641f787aeb05f539d31cbbb432d9600" {
		t.Fatalf("baseline body checksum drifted to %s; the baseline identity is frozen at release", got)
	}
}

// TestBridgeShapeGoldensMatchFrozenChain pins the golden shape files against
// the frozen executable chain: a golden that drifted from the released
// bodies would either strand real databases or hide a hand-edited one.
func TestBridgeShapeGoldensMatchFrozenChain(t *testing.T) {
	entries, err := bridgeShapeGoldenFS.ReadDir("testdata/golden-schema")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 14 {
		t.Fatalf("golden count = %d, want 14 (v15-v28)", len(entries))
	}
	keys := map[int]bool{}
	for _, entry := range entries {
		var version int
		if _, err := fmt.Sscanf(entry.Name(), "v%d.txt", &version); err != nil {
			t.Fatal(err)
		}
		if version < 15 || version > 28 {
			t.Fatalf("golden version %d outside the bridge window", version)
		}
		keys[version] = true
		var db *sql.DB
		if version == 28 {
			body, err := os.ReadFile("baseline_v28.sql")
			if err != nil {
				t.Fatal(err)
			}
			db, err = sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(string(body)); err != nil {
				t.Fatal(err)
			}
		} else {
			db = buildLegacyDB(t, version)
		}
		rows, err := db.Query(`SELECT type || ' ' || name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				t.Fatal(err)
			}
			got[key] = true
		}
		rows.Close()
		db.Close()
		if len(got) != len(bridgeShapeGoldens[version]) {
			t.Fatalf("golden %d has %d objects, frozen chain has %d", version, len(bridgeShapeGoldens[version]), len(got))
		}
		for key := range got {
			if !bridgeShapeGoldens[version][key] {
				t.Fatalf("golden %d missing released object %s", version, key)
			}
		}
	}
	for version := 15; version <= 28; version++ {
		if !keys[version] {
			t.Fatalf("no golden recorded for bridge version %d", version)
		}
	}
}
