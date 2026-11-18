// Package deployment owns deployment definitions and durable operation claims.
// It does not execute Docker commands or hold GPU/SSH credentials.
package deployment

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var ErrConflict = errors.New("deployment operation conflicts with recorded state")
var ErrInvalid = errors.New("invalid deployment definition or operation")
var ErrNotFound = errors.New("deployment or task not found")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$`)
var imagePattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var secretName = regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|API_KEY|ACCESS_KEY|CREDENTIALS?)($|_)`)
var secretRef = regexp.MustCompile(`^\$\{[A-Z0-9_]+(:\?[^}]*)?\}$`)

// Definitions carry canonical JSON as strings, so Python and Go hash identical
// bytes (including Unicode escaping and number representation).
type Definition struct {
	ManifestJSON  string `json:"manifest_json"`
	VersionJSON   string `json:"version_json"`
	ComposeSource string `json:"compose_source"`
	PredecessorID string `json:"predecessor_id,omitempty"`
}
type Submit struct {
	ID           string      `json:"id"`
	DeploymentID string      `json:"deployment_id"`
	Action       string      `json:"action"`
	Activate     bool        `json:"activate"`
	Definition   *Definition `json:"definition,omitempty"`
}
type Manifest struct {
	ID      string         `json:"deployment_id"`
	Model   string         `json:"model_id"`
	Version map[string]any `json:"version"`
	Target  struct {
		Compose    string `json:"compose"`
		ComposeSHA string `json:"compose_sha256"`
		Hostname   string `json:"hostname"`
		GPU        string `json:"gpu_uuid"`
		Instance   struct {
			ID       string          `json:"instance_id"`
			Model    string          `json:"model_key"`
			Machine  string          `json:"machine_id"`
			GPUs     []int           `json:"gpu_devices"`
			Protocol string          `json:"protocol"`
			Schema   json.RawMessage `json:"input_schema"`
		} `json:"instance"`
	} `json:"target"`
}
type Record struct {
	ID            string          `json:"id"`
	Manifest      json.RawMessage `json:"manifest"`
	ManifestSHA   string          `json:"manifest_sha256"`
	VersionSHA    string          `json:"version_sha256"`
	ComposeSource string          `json:"compose_source"`
	PredecessorID string          `json:"predecessor_id"`
	Phase         string          `json:"phase"`
	Events        []Event         `json:"events"`
}
type Event struct {
	ID           string         `json:"event_id"`
	DeploymentID string         `json:"deployment_id"`
	Phase        string         `json:"phase"`
	Summary      map[string]any `json:"summary"`
	CreatedAt    *time.Time     `json:"created_at,omitempty"`
}
type Task struct {
	ID              string          `json:"id"`
	DeploymentID    string          `json:"deployment_id"`
	Action          string          `json:"action"`
	ExecutionAction string          `json:"execution_action"`
	Activate        bool            `json:"activate"`
	Status          string          `json:"status"`
	Machine         string          `json:"machine_id"`
	Hostname        string          `json:"hostname"`
	ClaimToken      string          `json:"claim_token,omitempty"`
	Attempt         int             `json:"attempt"`
	Heartbeat       *time.Time      `json:"heartbeat_at"`
	Result          json.RawMessage `json:"result"`
}
type Store struct{ db *sql.DB }

func Open(dsn string) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid deployment database configuration")
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Ready(ctx context.Context) error {
	var n int
	return s.db.QueryRowContext(ctx, `SELECT count(events) FROM model_gateway.model_deployment_tasks WHERE false`).Scan(&n)
}
func hash(s string) string           { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }
func invalid(message string) error   { return fmt.Errorf("%w: %s", ErrInvalid, message) }
func decode(raw []byte, v any) error { return json.Unmarshal(raw, v) }
func validDefinition(d *Definition) (Manifest, error) {
	var m Manifest
	if len(d.ManifestJSON) > 2<<20 || len(d.ComposeSource) > 1<<20 || decode([]byte(d.ManifestJSON), &m) != nil {
		return m, invalid("malformed definition")
	}
	if !idPattern.MatchString(m.ID) || m.ID != m.Target.Instance.ID || m.Model == "" || m.Model != m.Target.Instance.Model {
		return m, invalid("identity mismatch")
	}
	if m.Target.Hostname == "" || m.Target.Instance.Machine == "" || !strings.HasPrefix(m.Target.GPU, "GPU-") || len(m.Target.Instance.GPUs) != 1 || m.Target.Instance.GPUs[0] < 0 {
		return m, invalid("explicit single-host/GPU target required")
	}
	if m.Target.Compose == "" || filepath.Base(m.Target.Compose) != m.Target.Compose || strings.ContainsAny(m.Target.Compose, "/\\") || m.Target.Compose == "manifest.json" || m.Target.Compose == "." || m.Target.Compose == ".." {
		return m, invalid("invalid Compose filename")
	}
	if !shaPattern.MatchString(m.Target.ComposeSHA) || hash(d.ComposeSource) != m.Target.ComposeSHA {
		return m, invalid("Compose digest mismatch")
	}
	var v map[string]any
	if decode([]byte(d.VersionJSON), &v) != nil || !reflect.DeepEqual(v, m.Version) {
		return m, invalid("version definition mismatch")
	}
	image, _ := m.Version["image"].(string)
	base, _ := m.Version["base_model"].(string)
	validator, _ := m.Version["output_validator"].(string)
	if !imagePattern.MatchString(image) || base == "" || validator == "" {
		return m, invalid("pinned image, base model and output validator required")
	}
	assets, ok := m.Version["assets"].([]any)
	if !ok || len(assets) == 0 {
		return m, invalid("pinned assets required")
	}
	names := map[string]bool{}
	hasValidator, hasWeights := false, false
	for _, entry := range assets {
		asset, ok := entry.(map[string]any)
		if !ok {
			return m, invalid("invalid asset")
		}
		name, _ := asset["name"].(string)
		sha, _ := asset["sha256"].(string)
		source, _ := asset["source"].(string)
		if name == "" || names[name] || !shaPattern.MatchString(sha) || source == "" {
			return m, invalid("asset name, source and SHA256 required")
		}
		names[name] = true
		hasValidator = hasValidator || (name == validator && asset["role"] == "validator")
		hasWeights = hasWeights || asset["role"] == "weights" || asset["role"] == "lora"
	}
	if !hasValidator || !hasWeights {
		return m, invalid("weight and validator assets required")
	}
	var c struct {
		Services map[string]struct {
			Environment map[string]any `json:"environment"`
		} `json:"services"`
	}
	if decode([]byte(d.ComposeSource), &c) != nil || len(c.Services) != 1 {
		return m, invalid("one Compose service required")
	}
	for _, service := range c.Services {
		for k, v := range service.Environment {
			if secretName.MatchString(k) && v != nil && v != "" && !secretRef.MatchString(fmt.Sprint(v)) {
				return m, invalid("inline credential in Compose")
			}
		}
	}
	if env, ok := m.Version["environment"].(map[string]any); ok {
		for k := range env {
			if secretName.MatchString(k) {
				return m, invalid("credentials cannot be part of version")
			}
		}
	}
	if assets, ok := m.Version["assets"].([]any); ok {
		for _, a := range assets {
			asset, ok := a.(map[string]any)
			if !ok {
				return m, invalid("invalid asset")
			}
			source, _ := asset["source"].(string)
			u, err := url.Parse(source)
			if err != nil || u.User != nil || u.RawQuery != "" {
				return m, invalid("asset sources must not contain credentials or signed query strings")
			}
		}
	}
	return m, nil
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func record(ctx context.Context, q querier, id string) (Record, error) {
	var r Record
	err := q.QueryRowContext(ctx, `SELECT id,manifest,manifest_sha256,version_sha256,compose_source,coalesce(predecessor_id,''),phase FROM model_gateway.model_deployments WHERE id=$1`, id).Scan(&r.ID, &r.Manifest, &r.ManifestSHA, &r.VersionSHA, &r.ComposeSource, &r.PredecessorID, &r.Phase)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	r, err := record(ctx, s.db, id)
	if err != nil {
		return r, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.entry FROM model_gateway.model_deployment_tasks t
 JOIN model_gateway.model_deployments d ON d.id=t.deployment_id
 CROSS JOIN LATERAL jsonb_array_elements(t.events) WITH ORDINALITY AS e(entry,position)
 WHERE (d.id=$1 OR d.predecessor_id=$1) AND e.entry->>'deployment_id'=$1
 ORDER BY t.created_at,t.id,e.position`, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	r.Events = []Event{}
	for rows.Next() {
		var e Event
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return r, err
		}
		if err = json.Unmarshal(raw, &e); err != nil {
			return r, err
		}
		r.Events = append(r.Events, e)
	}
	return r, rows.Err()
}
func task(ctx context.Context, q querier, id string, lock bool) (Task, error) {
	var t Task
	query := `SELECT id,deployment_id,action,execution_action,activate,status,machine_id,hostname,coalesce(claim_token,''),attempt,heartbeat_at,result FROM model_gateway.model_deployment_tasks WHERE id=$1`
	if lock {
		query += " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, query, id).Scan(&t.ID, &t.DeploymentID, &t.Action, &t.ExecutionAction, &t.Activate, &t.Status, &t.Machine, &t.Hostname, &t.ClaimToken, &t.Attempt, &t.Heartbeat, &t.Result)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return t, err
}
func (s *Store) Task(ctx context.Context, id string) (Task, error) {
	t, e := task(ctx, s.db, id, false)
	t.ClaimToken = ""
	return t, e
}
func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Short metadata transactions only. Consistent locking across Gateway replicas.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(72104982061118)`); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Submit(ctx context.Context, in Submit) (Task, error) {
	if !idPattern.MatchString(in.ID) || !idPattern.MatchString(in.DeploymentID) {
		return Task{}, invalid("invalid task/deployment ID")
	}
	switch in.Action {
	case "deploy", "switch-lora", "rollback", "stop", "adopt":
	default:
		return Task{}, invalid("unsupported action")
	}
	if in.Activate && in.Action != "deploy" {
		return Task{}, invalid("activate only applies to deploy")
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if existing, e := task(ctx, tx, in.ID, false); e == nil {
			if existing.DeploymentID != in.DeploymentID || existing.Action != in.Action || existing.Activate != in.Activate {
				return ErrConflict
			}
			if in.Definition != nil {
				r, e := record(ctx, tx, in.DeploymentID)
				if e != nil {
					return e
				}
				if r.ManifestSHA != hash(in.Definition.ManifestJSON) || r.VersionSHA != hash(in.Definition.VersionJSON) || r.ComposeSource != in.Definition.ComposeSource || r.PredecessorID != in.Definition.PredecessorID {
					return ErrConflict
				}
			}
			return nil
		} else if !errors.Is(e, ErrNotFound) {
			return e
		}
		r, e := record(ctx, tx, in.DeploymentID)
		if errors.Is(e, ErrNotFound) {
			if in.Definition == nil || !(in.Action == "deploy" || in.Action == "switch-lora" || in.Action == "adopt") {
				return invalid("new deployment needs a definition")
			}
			m, validationErr := validDefinition(in.Definition)
			if validationErr != nil {
				return validationErr
			}
			if m.ID != in.DeploymentID {
				return invalid("deployment ID mismatch")
			}
			if (in.Action == "switch-lora") != (in.Definition.PredecessorID != "") {
				return invalid("only switch-lora declares a predecessor")
			}
			if in.Definition.PredecessorID != "" {
				old, e := record(ctx, tx, in.Definition.PredecessorID)
				if e != nil {
					return e
				}
				var a Manifest
				_ = decode(old.Manifest, &a)
				var sa, sb any
				_ = decode(a.Target.Instance.Schema, &sa)
				_ = decode(m.Target.Instance.Schema, &sb)
				if a.Model != m.Model || a.Target.Hostname != m.Target.Hostname || a.Target.Instance.Machine != m.Target.Instance.Machine || a.Target.Instance.Protocol != m.Target.Instance.Protocol || !reflect.DeepEqual(sa, sb) {
					return invalid("predecessor model, machine and contract must match")
				}
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO model_gateway.model_deployments(id,manifest_sha256,version_sha256,model_key,machine_id,gpu_device,gpu_uuid,manifest,compose_source,predecessor_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,NULLIF($10,''))`, m.ID, hash(in.Definition.ManifestJSON), hash(in.Definition.VersionJSON), m.Model, m.Target.Instance.Machine, m.Target.Instance.GPUs[0], m.Target.GPU, in.Definition.ManifestJSON, in.Definition.ComposeSource, in.Definition.PredecessorID)
			if e != nil {
				return e
			}
			r, e = record(ctx, tx, in.DeploymentID)
		}
		if e != nil {
			return e
		}
		if in.Definition != nil && (r.ManifestSHA != hash(in.Definition.ManifestJSON) || r.VersionSHA != hash(in.Definition.VersionJSON) || r.ComposeSource != in.Definition.ComposeSource || r.PredecessorID != in.Definition.PredecessorID) {
			return ErrConflict
		}
		if in.Action == "switch-lora" || in.Action == "rollback" {
			if r.PredecessorID == "" {
				return invalid("recorded predecessor required")
			}
		}
		if in.Action == "deploy" && r.PredecessorID != "" {
			return invalid("replacement requires switch-lora")
		}
		if (in.Action == "deploy" || in.Action == "switch-lora" || in.Action == "adopt") && (r.Phase == "stopped" || r.Phase == "rolled_back" || r.Phase == "active") {
			return ErrConflict
		}
		var m Manifest
		_ = decode(r.Manifest, &m)
		gpus := []string{m.Target.GPU}
		if r.PredecessorID != "" {
			old, e := record(ctx, tx, r.PredecessorID)
			if e != nil {
				return e
			}
			var a Manifest
			_ = decode(old.Manifest, &a)
			if a.Target.GPU != m.Target.GPU {
				gpus = append(gpus, a.Target.GPU)
			}
		}
		// Reject unrelated live deployment metadata, including stopped model history for a fresh ID.
		rows, e := tx.QueryContext(ctx, `SELECT id,model_key,phase FROM model_gateway.model_deployments WHERE id<>$1 AND id<>$2 AND (model_key=$3 OR gpu_uuid=$4)`, r.ID, r.PredecessorID, m.Model, m.Target.GPU)
		if e != nil {
			return e
		}
		for rows.Next() {
			var id, model, phase string
			if e = rows.Scan(&id, &model, &phase); e != nil {
				rows.Close()
				return e
			}
			if (model == m.Model && r.PredecessorID == "") || (phase != "stopped" && phase != "rolled_back" && phase != "superseded") {
				rows.Close()
				return ErrConflict
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		// The unfinished task IS the durable reservation. The transaction advisory
		// lock serializes this check with submissions/finishes on every API replica.
		var busy bool
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM model_gateway.model_deployment_tasks t
 JOIN model_gateway.model_deployments d ON d.id=t.deployment_id
 LEFT JOIN model_gateway.model_deployments p ON p.id=d.predecessor_id
 WHERE t.status IN ('queued','running','recovery_required')
 AND (d.model_key=$1 OR d.gpu_uuid=ANY($2::text[]) OR p.gpu_uuid=ANY($2::text[])))`, m.Model, gpus).Scan(&busy)
		if e != nil {
			return e
		}
		if busy {
			return ErrConflict
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO model_gateway.model_deployment_tasks(id,deployment_id,action,execution_action,activate,machine_id,hostname) VALUES($1,$2,$3,$3,$4,$5,$6)`, in.ID, r.ID, in.Action, in.Activate, m.Target.Instance.Machine, m.Target.Hostname)
		if e != nil {
			return e
		}

		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return s.Task(ctx, in.ID)
}
func (s *Store) Claim(ctx context.Context, id, machine, hostname string, recover bool) (Task, error) {
	var out Task
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		t, e := task(ctx, tx, id, true)
		if e != nil {
			return e
		}
		if t.Machine != machine || t.Hostname != hostname {
			return ErrConflict
		}
		action := t.Action
		if recover {
			if t.Status != "running" && t.Status != "recovery_required" {
				return ErrConflict
			}
			r, e := record(ctx, tx, t.DeploymentID)
			if e != nil {
				return e
			}
			action = "stop"
			if r.PredecessorID != "" {
				action = "rollback"
			}
		} else if t.Status != "queued" {
			return ErrConflict
		}
		token := uuid.NewString()
		_, e = tx.ExecContext(ctx, `UPDATE model_gateway.model_deployment_tasks SET status='running',claim_token=$2,attempt=attempt+1,heartbeat_at=now(),updated_at=now(),execution_action=$3 WHERE id=$1`, id, token, action)
		if e != nil {
			return e
		}
		out, e = task(ctx, tx, id, false)
		return e
	})
	return out, err
}
func owned(ctx context.Context, tx *sql.Tx, id, token string) (Task, error) {
	t, e := task(ctx, tx, id, true)
	if e != nil {
		return t, e
	}
	if t.Status != "running" || token == "" || t.ClaimToken != token {
		return t, ErrConflict
	}
	return t, nil
}
func (s *Store) Heartbeat(ctx context.Context, id, token string) error {
	r, e := s.db.ExecContext(ctx, `UPDATE model_gateway.model_deployment_tasks SET heartbeat_at=now(),updated_at=now() WHERE id=$1 AND status='running' AND claim_token=$2`, id, token)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e == nil && n != 1 {
		return ErrConflict
	}
	return e
}

var phases = map[string]bool{"deploy_intent": true, "start_intent": true, "started": true, "staged": true, "validation_intent": true, "acceptance_intent": true, "acceptance_passed": true, "validated": true, "activation_intent": true, "active": true, "cutover_drain_intent": true, "stop_intent": true, "stopped": true, "switch_intent": true, "rollback_intent": true, "rollback_restoring": true, "rolled_back": true, "adopt_intent": true}

func (s *Store) Event(ctx context.Context, id, token string, e Event) error {
	if !idPattern.MatchString(e.ID) || !phases[e.Phase] {
		return invalid("invalid event")
	}
	if e.Summary == nil {
		e.Summary = map[string]any{}
	}
	raw, err := json.Marshal(e.Summary)
	if err != nil || len(raw) > 256<<10 {
		return invalid("invalid event summary")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		t, err := owned(ctx, tx, id, token)
		if err != nil {
			return err
		}
		r, err := record(ctx, tx, t.DeploymentID)
		if err != nil {
			return err
		}
		if e.DeploymentID != r.ID && e.DeploymentID != r.PredecessorID {
			return ErrConflict
		}
		var prevPhase, prevID string
		var prevRaw []byte
		err = tx.QueryRowContext(ctx, `SELECT e->>'deployment_id',e->>'phase',e->'summary' FROM model_gateway.model_deployment_tasks t, jsonb_array_elements(t.events) e WHERE t.id=$1 AND e->>'event_id'=$2`, id, e.ID).Scan(&prevID, &prevPhase, &prevRaw)
		if err == nil {
			var a, b any
			_ = decode(prevRaw, &a)
			_ = decode(raw, &b)
			if prevID != e.DeploymentID || prevPhase != e.Phase || !reflect.DeepEqual(a, b) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if e.Phase == "active" {
			if e.DeploymentID == r.ID {
				proof := "validated"
				if t.ExecutionAction == "adopt" {
					proof = "acceptance_passed"
				}
				var accepted bool
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_gateway.model_deployment_tasks t, jsonb_array_elements(t.events) e WHERE t.id=$1 AND e->>'deployment_id'=$2 AND e->>'phase'=$3)`, id, r.ID, proof).Scan(&accepted); err != nil {
					return err
				}
				if !accepted || (t.ExecutionAction != "deploy" && t.ExecutionAction != "switch-lora" && t.ExecutionAction != "adopt") {
					return ErrConflict
				}
			} else if r.Phase != "rollback_restoring" {
				return ErrConflict
			}
		}
		if e.Phase == "active" {
			// Rollback activates the predecessor; normal cutover activates the candidate.
			other := r.PredecessorID
			if e.DeploymentID == r.PredecessorID {
				other = r.ID
			}
			if _, err = tx.ExecContext(ctx, `UPDATE model_gateway.model_deployments SET phase='draining',updated_at=now() WHERE id=$1 AND phase='active'`, other); err != nil {
				return err
			}
		}
		e.CreatedAt = nil // Time is assigned by the database, never by the executor.
		entry, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE model_gateway.model_deployment_tasks SET events=events || jsonb_build_array($2::jsonb || jsonb_build_object('created_at',clock_timestamp())),updated_at=now() WHERE id=$1`, id, string(entry)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE model_gateway.model_deployments SET phase=$2,updated_at=now() WHERE id=$1`, e.DeploymentID, e.Phase)
		return err
	})
}
func (s *Store) Finish(ctx context.Context, id, token, status string, result json.RawMessage) error {
	if status != "succeeded" && status != "failed" && status != "recovery_required" {
		return invalid("invalid task outcome")
	}
	if !json.Valid(result) || len(result) > 256<<10 {
		return invalid("invalid result")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		t, err := task(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if token == "" || t.ClaimToken != token {
			return ErrConflict
		}
		if t.Status != "running" {
			if t.Status == status && jsonEqual(t.Result, result) {
				return nil
			}
			return ErrConflict
		}
		r, err := record(ctx, tx, t.DeploymentID)
		if err != nil {
			return err
		}
		// A failed operation only releases locks after a recorded safe stop/rollback.
		if status == "failed" {
			restored := r.Phase == "rolled_back"
			stopped := r.Phase == "stopped" && t.ExecutionAction != "switch-lora" && t.ExecutionAction != "rollback"
			if !restored && !stopped {
				return ErrConflict
			}
		}
		if status == "succeeded" {
			expected := "active"
			switch t.ExecutionAction {
			case "stop":
				expected = "stopped"
			case "rollback":
				expected = "rolled_back"
			case "deploy":
				if !t.Activate {
					expected = "staged"
				}
			}
			if r.Phase != expected {
				return ErrConflict
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE model_gateway.model_deployment_tasks SET status=$2,result=$3::jsonb,updated_at=now() WHERE id=$1`, id, status, string(result)); err != nil {
			return err
		}
		return err
	})
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	return decode(a, &x) == nil && decode(b, &y) == nil && reflect.DeepEqual(x, y)
}
