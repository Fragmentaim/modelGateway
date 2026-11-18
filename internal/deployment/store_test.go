package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func fixture(id string) *Definition {
	compose := `{"services":{"worker":{"image":"registry/worker@sha256:abc","environment":{"API_TOKEN":"${API_TOKEN}"}}}}`
	version := map[string]any{"image": "registry/worker@sha256:" + strings.Repeat("a", 64), "engine": "qwen", "base_model": "pinned-base", "output_validator": "validator", "environment": map[string]any{}, "assets": []any{map[string]any{"name": "weights", "role": "weights", "source": "hf://org/model@revision/weights", "sha256": strings.Repeat("a", 64)}, map[string]any{"name": "validator", "role": "validator", "source": "git://example/validator.py", "sha256": strings.Repeat("b", 64)}}}
	m := map[string]any{"deployment_id": id, "model_id": "stable-model", "version": version, "target": map[string]any{"compose": "compose.json", "compose_sha256": hash(compose), "hostname": "gpu-host", "gpu_uuid": "GPU-physical", "instance": map[string]any{"instance_id": id, "model_key": "stable-model", "machine_id": "i-test", "gpu_devices": []int{0}, "protocol": "image_generation", "input_schema": map[string]any{"type": "object"}}}}
	raw, _ := json.Marshal(m)
	v, _ := json.Marshal(version)
	return &Definition{ManifestJSON: string(raw), VersionJSON: string(v), ComposeSource: compose}
}
func setup(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DEPLOY_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DEPLOY_DATABASE_URL required")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil || !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatal("disposable _test database required")
	}
	s, e := Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if _, e = s.db.ExecContext(t.Context(), `DROP SCHEMA IF EXISTS model_deploy CASCADE; DROP TABLE IF EXISTS model_gateway.model_deployment_tasks, model_gateway.model_deployments CASCADE`); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile("../../deploy/migrations/model-deployments.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.ExecContext(t.Context(), string(raw)); e != nil {
		t.Fatal(e)
	}
	return s
}
func submit(t *testing.T, s *Store, id, dep, action, prev string) Task {
	t.Helper()
	d := fixture(dep)
	d.PredecessorID = prev
	in := Submit{ID: id, DeploymentID: dep, Action: action, Activate: action == "deploy"}
	if action == "deploy" || action == "switch-lora" {
		in.Definition = d
	}
	task, e := s.Submit(t.Context(), in)
	if e != nil {
		t.Fatal(e)
	}
	return task
}
func claim(t *testing.T, s *Store, id string) Task {
	t.Helper()
	task, e := s.Claim(t.Context(), id, "i-test", "gpu-host", false)
	if e != nil {
		t.Fatal(e)
	}
	return task
}
func event(t *testing.T, s *Store, task Task, dep, phase string) {
	t.Helper()
	if e := s.Event(t.Context(), task.ID, task.ClaimToken, Event{ID: dep + "-" + phase, DeploymentID: dep, Phase: phase, Summary: map[string]any{}}); e != nil {
		t.Fatal(e)
	}
}
func active(t *testing.T, s *Store) {
	submit(t, s, "job-a", "a", "deploy", "")
	a := claim(t, s, "job-a")
	event(t, s, a, "a", "validated")
	event(t, s, a, "a", "active")
	if e := s.Finish(t.Context(), a.ID, a.ClaimToken, "succeeded", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
}
func TestDefinitionBoundaries(t *testing.T) {
	d := fixture("a")
	if _, e := validDefinition(d); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*Definition){func(d *Definition) { d.ComposeSource += " " }, func(d *Definition) { d.VersionJSON = `{}` }, func(d *Definition) {
		d.ManifestJSON = strings.ReplaceAll(d.ManifestJSON, "compose.json", "../compose.json")
	}, func(d *Definition) { d.ManifestJSON = strings.ReplaceAll(d.ManifestJSON, "GPU-physical", "unknown") }} {
		d := fixture("a")
		change(d)
		if _, e := validDefinition(d); !errors.Is(e, ErrInvalid) {
			t.Fatal("accepted malformed definition", e)
		}
	}
	d = fixture("a")
	d.ComposeSource = strings.ReplaceAll(d.ComposeSource, "${API_TOKEN}", "inline-secret")
	var m map[string]any
	json.Unmarshal([]byte(d.ManifestJSON), &m)
	m["target"].(map[string]any)["compose_sha256"] = hash(d.ComposeSource)
	raw, _ := json.Marshal(m)
	d.ManifestJSON = string(raw)
	if _, e := validDefinition(d); !errors.Is(e, ErrInvalid) {
		t.Fatal("accepted inline secret", e)
	}
}
func TestSubmitIdempotencyAndNoPartialRecords(t *testing.T) {
	s := setup(t)
	a := submit(t, s, "job-a", "a", "deploy", "")
	retry := submit(t, s, "job-a", "a", "deploy", "")
	if a.ID != retry.ID {
		t.Fatal("duplicate task")
	}
	d := fixture("b")
	if _, e := s.Submit(t.Context(), Submit{ID: "job-b", DeploymentID: "b", Action: "deploy", Definition: d}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, e := s.Get(t.Context(), "b"); !errors.Is(e, ErrNotFound) {
		t.Fatal("failed submit left a record", e)
	}
	if _, e := s.Submit(t.Context(), Submit{ID: "job-a", DeploymentID: "a", Action: "stop"}); !errors.Is(e, ErrConflict) {
		t.Fatal("task identity reused", e)
	}
}
func TestConcurrentOperationsHaveOneOwner(t *testing.T) {
	s := setup(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, e := s.Submit(context.Background(), Submit{ID: "job-" + id, DeploymentID: id, Action: "deploy", Definition: fixture(id)})
			results <- e
		}(id)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
}
func TestClaimFencingAndNoAutomaticRequeue(t *testing.T) {
	s := setup(t)
	submit(t, s, "job-a", "a", "deploy", "")
	if _, e := s.Claim(t.Context(), "job-a", "another-machine", "gpu-host", false); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	a := claim(t, s, "job-a")
	s.db.Exec(`UPDATE model_gateway.model_deployment_tasks SET heartbeat_at=now()-interval '1 day'`)
	if _, e := s.Claim(t.Context(), a.ID, "i-test", "gpu-host", false); !errors.Is(e, ErrConflict) {
		t.Fatal("stale claim auto retried", e)
	}
	public, e := s.Task(t.Context(), a.ID)
	if e != nil || public.ClaimToken != "" {
		t.Fatal("public task exposed claim", e)
	}
	recovered, e := s.Claim(t.Context(), a.ID, "i-test", "gpu-host", true)
	if e != nil || recovered.ExecutionAction != "stop" || recovered.ClaimToken == a.ClaimToken {
		t.Fatal(recovered, e)
	}
	if e = s.Heartbeat(t.Context(), a.ID, a.ClaimToken); !errors.Is(e, ErrConflict) {
		t.Fatal("old executor not fenced", e)
	}
	if e = s.Event(t.Context(), a.ID, a.ClaimToken, Event{ID: "stale", DeploymentID: "a", Phase: "active"}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}
func TestEventsAndFinishAreIdempotent(t *testing.T) {
	s := setup(t)
	submit(t, s, "job-a", "a", "deploy", "")
	a := claim(t, s, "job-a")
	event(t, s, a, "a", "validated")
	event(t, s, a, "a", "active")
	event(t, s, a, "a", "validated")
	event(t, s, a, "a", "active")
	r, e := s.Get(t.Context(), "a")
	if e != nil || len(r.Events) != 2 {
		t.Fatal(r, e)
	}
	e = s.Event(t.Context(), a.ID, a.ClaimToken, Event{ID: "a-active", DeploymentID: "a", Phase: "active", Summary: map[string]any{"different": true}})
	if !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	e = s.Event(t.Context(), a.ID, a.ClaimToken, Event{ID: "unrelated", DeploymentID: "other", Phase: "active"})
	if !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	for range 2 {
		if e = s.Finish(t.Context(), a.ID, a.ClaimToken, "succeeded", json.RawMessage(`{"ok":true}`)); e != nil {
			t.Fatal(e)
		}
	}
}
func TestSuccessfulSwitchAndRollbackKeepHistory(t *testing.T) {
	s := setup(t)
	active(t, s)
	submit(t, s, "job-b", "b", "switch-lora", "a")
	b := claim(t, s, "job-b")
	event(t, s, b, "b", "validated")
	event(t, s, b, "b", "active")
	event(t, s, b, "a", "stopped")
	if e := s.Finish(t.Context(), b.ID, b.ClaimToken, "succeeded", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	submit(t, s, "job-rollback", "b", "rollback", "")
	rb := claim(t, s, "job-rollback")
	event(t, s, rb, "b", "stopped")
	event(t, s, rb, "b", "rollback_restoring")
	event(t, s, rb, "a", "active")
	event(t, s, rb, "b", "rolled_back")
	if e := s.Finish(t.Context(), rb.ID, rb.ClaimToken, "succeeded", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	a, _ := s.Get(t.Context(), "a")
	next, _ := s.Get(t.Context(), "b")
	if a.Phase != "active" || next.Phase != "rolled_back" || next.PredecessorID != "a" {
		t.Fatal(a, next)
	}
	if len(a.Events) != 4 || a.Events[2].Phase != "stopped" || a.Events[3].Phase != "active" || a.Events[3].CreatedAt == nil {
		t.Fatal("predecessor history lost cross-task events or server timestamps", a.Events)
	}
}
func TestUnknownExecutionRetainsLocksUntilRecovery(t *testing.T) {
	s := setup(t)
	active(t, s)
	submit(t, s, "job-b", "b", "switch-lora", "a")
	b := claim(t, s, "job-b")
	if e := s.Finish(t.Context(), b.ID, b.ClaimToken, "failed", json.RawMessage(`{}`)); !errors.Is(e, ErrConflict) {
		t.Fatal("unsafe failure released ownership", e)
	}
	if e := s.Finish(t.Context(), b.ID, b.ClaimToken, "recovery_required", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Submit(t.Context(), Submit{ID: "job-stop", DeploymentID: "a", Action: "stop"}); !errors.Is(e, ErrConflict) {
		t.Fatal("unknown execution unlocked resource", e)
	}
	rb, e := s.Claim(t.Context(), b.ID, "i-test", "gpu-host", true)
	if e != nil || rb.ExecutionAction != "rollback" {
		t.Fatal(rb, e)
	}
	event(t, s, rb, "b", "stopped")
	event(t, s, rb, "b", "rollback_restoring")
	event(t, s, rb, "a", "active")
	event(t, s, rb, "b", "rolled_back")
	if e = s.Finish(t.Context(), rb.ID, rb.ClaimToken, "succeeded", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
}
func TestDefinitionsCannotBeOverwritten(t *testing.T) {
	s := setup(t)
	submit(t, s, "job-a", "a", "deploy", "")
	if _, e := s.db.Exec(`UPDATE model_gateway.model_deployments SET manifest='{}' WHERE id='a'`); e == nil {
		t.Fatal("mutable definition")
	}
	if _, e := s.db.Exec(`UPDATE model_gateway.model_deployments SET version_sha256='changed'`); e == nil {
		t.Fatal("mutable version")
	}
	d := fixture("a")
	d.ManifestJSON += " "
	if _, e := s.Submit(t.Context(), Submit{ID: "job-a", DeploymentID: "a", Action: "deploy", Activate: true, Definition: d}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestActivationRequiresAcceptanceProof(t *testing.T) {
	s := setup(t)
	submit(t, s, "job-a", "a", "deploy", "")
	a := claim(t, s, "job-a")
	if err := s.Event(t.Context(), a.ID, a.ClaimToken, Event{ID: "premature", DeploymentID: "a", Phase: "active"}); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted activation before validation", err)
	}
}

func TestDocumentedRuntimeGrants(t *testing.T) {
	owner := setup(t)
	_, err := owner.db.Exec(`DO $$ BEGIN IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='model_deploy_test_runtime') THEN CREATE ROLE model_deploy_test_runtime; END IF; END $$;
 GRANT USAGE ON SCHEMA model_gateway TO model_deploy_test_runtime;
 GRANT SELECT, INSERT ON model_gateway.model_deployments, model_gateway.model_deployment_tasks TO model_deploy_test_runtime;
 GRANT UPDATE(phase,updated_at) ON model_gateway.model_deployments TO model_deploy_test_runtime;
 GRANT UPDATE(execution_action,status,claim_token,attempt,heartbeat_at,result,events,updated_at) ON model_gateway.model_deployment_tasks TO model_deploy_test_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(os.Getenv("TEST_DEPLOY_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["role"] = "model_deploy_test_runtime"
	s := &Store{db: stdlib.OpenDB(*cfg)}
	defer s.Close()
	if err = s.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	active(t, s)
	if _, err = s.db.Exec(`DELETE FROM model_gateway.model_deployments`); err == nil {
		t.Fatal("runtime can delete history")
	}
	if _, err = s.db.Exec(`UPDATE model_gateway.model_deployments SET version_sha256='changed'`); err == nil {
		t.Fatal("runtime can overwrite version")
	}
}

func TestStoppedCandidateAloneDoesNotProveRollback(t *testing.T) {
	s := setup(t)
	active(t, s)
	submit(t, s, "job-b", "b", "switch-lora", "a")
	b := claim(t, s, "job-b")
	event(t, s, b, "a", "stopped")
	event(t, s, b, "b", "stopped")
	if err := s.Finish(t.Context(), b.ID, b.ClaimToken, "failed", json.RawMessage(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("failed switch released locks without restoring predecessor", err)
	}
}

func TestUnfinishedTaskProtectsBothGPUsWithoutLockTable(t *testing.T) {
	s := setup(t)
	active(t, s)
	change := func(d *Definition, model, gpu string) {
		d.ManifestJSON = strings.ReplaceAll(strings.ReplaceAll(d.ManifestJSON, "stable-model", model), "GPU-physical", gpu)
	}
	d := fixture("b")
	d.PredecessorID = "a"
	change(d, "stable-model", "GPU-spare")
	if _, err := s.Submit(t.Context(), Submit{ID: "job-b", DeploymentID: "b", Action: "switch-lora", Definition: d}); err != nil {
		t.Fatal(err)
	}
	b := claim(t, s, "job-b")
	event(t, s, b, "a", "stopped")
	event(t, s, b, "b", "stopped")
	if err := s.Finish(t.Context(), b.ID, b.ClaimToken, "recovery_required", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	for _, gpu := range []string{"GPU-physical", "GPU-spare"} {
		other := fixture("other")
		change(other, "another-model", gpu)
		if _, err := s.Submit(t.Context(), Submit{ID: "other-job", DeploymentID: "other", Action: "deploy", Definition: other}); !errors.Is(err, ErrConflict) {
			t.Fatal("stopped phase allowed a new operation on reserved GPU", gpu, err)
		}
	}
	rb, err := s.Claim(t.Context(), b.ID, "i-test", "gpu-host", true)
	if err != nil {
		t.Fatal(err)
	}
	event(t, s, rb, "b", "rollback_restoring")
	event(t, s, rb, "a", "active")
	event(t, s, rb, "b", "rolled_back")
	if err = s.Finish(t.Context(), rb.ID, rb.ClaimToken, "succeeded", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	other := fixture("other")
	change(other, "another-model", "GPU-spare")
	if _, err = s.Submit(t.Context(), Submit{ID: "other-job", DeploymentID: "other", Action: "deploy", Definition: other}); err != nil {
		t.Fatal("terminal task did not release spare GPU", err)
	}
}

func TestConcurrentEventAppendKeepsReceipts(t *testing.T) {
	s := setup(t)
	submit(t, s, "job-a", "a", "deploy", "")
	a := claim(t, s, "job-a")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for _, id := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			results <- s.Event(context.Background(), a.ID, a.ClaimToken, Event{ID: id, DeploymentID: "a", Phase: "started"})
		}(id)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Get(t.Context(), "a")
	if err != nil || len(r.Events) != 8 {
		t.Fatal("concurrent append lost receipts", len(r.Events), err)
	}
}

func TestMigrationKeepsOnlyTwoTablesAndPreservesExistingGateway(t *testing.T) {
	s := setup(t)
	legacy, err := os.ReadFile("testdata/legacy-model-deployments.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(string(legacy)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TABLE model_gateway.migration_sentinel(id int PRIMARY KEY); INSERT INTO model_gateway.migration_sentinel VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Exec(`DROP TABLE IF EXISTS model_gateway.migration_sentinel`) })
	migration, _ := os.ReadFile("../../deploy/migrations/model-deployments.sql")
	for attempt := range 2 {
		if _, err = s.db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			active(t, s)
		}
	}
	recorded, err := s.Get(t.Context(), "a")
	if err != nil || recorded.Phase != "active" || len(recorded.Events) != 2 {
		t.Fatal("repeat migration changed new deployment history", recorded, err)
	}
	var oldSchema, tables, sentinel int
	s.db.QueryRow(`SELECT count(*) FROM pg_namespace WHERE nspname='model_deploy'`).Scan(&oldSchema)
	s.db.QueryRow(`SELECT count(*) FROM pg_tables WHERE schemaname='model_gateway' AND tablename IN ('model_deployments','model_deployment_tasks')`).Scan(&tables)
	s.db.QueryRow(`SELECT count(*) FROM model_gateway.migration_sentinel`).Scan(&sentinel)
	if oldSchema != 0 || tables != 2 || sentinel != 1 {
		t.Fatal(oldSchema, tables, sentinel)
	}
}

func TestMigrationRefusesNonemptyLegacyTables(t *testing.T) {
	s := setup(t)
	legacy, _ := os.ReadFile("testdata/legacy-model-deployments.sql")
	if _, err := s.db.Exec(string(legacy)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO model_deploy.versions(sha256,definition) VALUES($1,'{"retained":true}')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	migration, _ := os.ReadFile("../../deploy/migrations/model-deployments.sql")
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(t.Context(), string(migration)); err == nil || !strings.Contains(err.Error(), "contains data") {
		t.Fatal("migration did not stop on legacy data", err)
	}
	conn.ExecContext(t.Context(), `ROLLBACK`)
	var count int
	if err = conn.QueryRowContext(t.Context(), `SELECT count(*) FROM model_deploy.versions WHERE definition->>'retained'='true'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("legacy history was lost", count, err)
	}
}

func TestMigrationCannotDropConcurrentLegacyInsert(t *testing.T) {
	s := setup(t)
	legacy, _ := os.ReadFile("testdata/legacy-model-deployments.sql")
	if _, err := s.db.Exec(string(legacy)); err != nil {
		t.Fatal(err)
	}
	writer, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err = writer.Exec(`INSERT INTO model_deploy.versions(sha256,definition) VALUES($1,'{"retained":true}')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pid int
	if err = conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	migration, _ := os.ReadFile("../../deploy/migrations/model-deployments.sql")
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		_, err := conn.ExecContext(ctx, string(migration))
		conn.ExecContext(context.Background(), `ROLLBACK`)
		result <- err
	}()
	waiting := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err = s.db.QueryRow(`SELECT coalesce(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("migration did not wait for the concurrent writer")
	}
	if err = writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err == nil || !strings.Contains(err.Error(), "contains data") {
		t.Fatal("migration did not preserve the concurrent insert", err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM model_deploy.versions WHERE definition->>'retained'='true'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent legacy record was lost", count, err)
	}
}
