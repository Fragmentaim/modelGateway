package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

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
	if _, e = s.db.ExecContext(t.Context(), `DROP SCHEMA IF EXISTS model_deploy CASCADE`); e != nil {
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
	s.db.Exec(`UPDATE model_deploy.tasks SET heartbeat_at=now()-interval '1 day'`)
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
	if _, e := s.db.Exec(`UPDATE model_deploy.deployments SET manifest='{}' WHERE id='a'`); e == nil {
		t.Fatal("mutable definition")
	}
	if _, e := s.db.Exec(`UPDATE model_deploy.versions SET definition='{}'`); e == nil {
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
 GRANT USAGE ON SCHEMA model_deploy TO model_deploy_test_runtime;
 GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA model_deploy TO model_deploy_test_runtime;
 GRANT UPDATE(phase,updated_at) ON model_deploy.deployments TO model_deploy_test_runtime;
 GRANT UPDATE(execution_action,status,claim_token,attempt,heartbeat_at,result,updated_at) ON model_deploy.tasks TO model_deploy_test_runtime;
 GRANT DELETE ON model_deploy.operation_locks TO model_deploy_test_runtime;
 GRANT USAGE ON ALL SEQUENCES IN SCHEMA model_deploy TO model_deploy_test_runtime;`)
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
	if _, err = s.db.Exec(`DELETE FROM model_deploy.deployments`); err == nil {
		t.Fatal("runtime can delete history")
	}
	if _, err = s.db.Exec(`UPDATE model_deploy.versions SET definition='{}'`); err == nil {
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
