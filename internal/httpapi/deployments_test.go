package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/jackc/pgx/v5"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/deployment"
)

func TestDeploymentAPIRequiresAdminAndExplicitEnable(t *testing.T) {
	cfg := config.Config{AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{"8001"}, APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24), ExperimentalToken: strings.Repeat("e", 24)}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h := New(nil, runtime, nil).HTTPServer(l)
	for _, path := range []string{"/v1/admin/deployments-ready", "/v1/admin/deployments/x", "/v1/admin/deployment-tasks/x"} {
		for _, token := range []string{cfg.APIToken, cfg.ExperimentalToken, "wrong"} {
			r := ut.PerformRequest(h.Engine, "GET", path, nil, ut.Header{Key: "Authorization", Value: "Bearer " + token})
			if r.Code != 401 {
				t.Fatal(path, r.Code)
			}
		}
		r := ut.PerformRequest(h.Engine, "GET", path, nil, ut.Header{Key: "Authorization", Value: "Bearer " + cfg.AdminToken})
		if r.Code != 503 || !strings.Contains(r.Body.String(), "deployments_disabled") {
			t.Fatal(r.Code, r.Body.String())
		}
	}
}
func TestDeploymentAPIWithPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DEPLOY_HTTP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DEPLOY_HTTP_DATABASE_URL required")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil || !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatal("test database required")
	}
	conn, e := pgx.ConnectConfig(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	if _, e = conn.Exec(t.Context(), `DROP SCHEMA IF EXISTS model_deploy CASCADE`); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile("../../deploy/migrations/model-deployments.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(t.Context(), string(raw)); e != nil {
		t.Fatal(e)
	}
	repo, e := deployment.Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	conf := config.Config{AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{"8001"}, APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24), DeploymentsEnabled: true}
	runtime, e := config.NewRuntime(conf)
	if e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	s := New(nil, runtime, nil)
	s.SetDeployments(repo)
	h := s.HTTPServer(l)
	call := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		var payload *ut.Body
		if body != nil {
			raw, _ := json.Marshal(body)
			payload = &ut.Body{Body: strings.NewReader(string(raw)), Len: len(raw)}
		}
		r := ut.PerformRequest(h.Engine, method, path, payload, ut.Header{Key: "Authorization", Value: "Bearer " + conf.AdminToken}, ut.Header{Key: "Content-Type", Value: "application/json"})
		if r.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, r.Code, r.Body.String())
		}
		var result map[string]any
		if e := json.Unmarshal(r.Body.Bytes(), &result); e != nil {
			t.Fatal(e)
		}
		return result
	}
	call("GET", "/v1/admin/deployments-ready", nil, 200)
	compose := `{"services":{"worker":{"environment":{}}}}`
	sha := sha256.Sum256([]byte(compose))
	versionRaw, _ := json.Marshal(map[string]any{"engine": "qwen", "base_model": "pinned-base", "image": "registry/image@sha256:" + strings.Repeat("a", 64), "output_validator": "validator", "assets": []any{map[string]any{"name": "weights", "role": "weights", "source": "s3://test/weights", "sha256": strings.Repeat("a", 64)}, map[string]any{"name": "validator", "role": "validator", "source": "git://test/validator", "sha256": strings.Repeat("b", 64)}}})
	version := string(versionRaw)
	manifest := map[string]any{"deployment_id": "api-deployment", "model_id": "api-model", "version": json.RawMessage(version), "target": map[string]any{"compose": "compose.json", "compose_sha256": hex.EncodeToString(sha[:]), "hostname": "gpu-host", "gpu_uuid": "GPU-api", "instance": map[string]any{"instance_id": "api-deployment", "model_key": "api-model", "machine_id": "i-api", "gpu_devices": []int{0}}}}
	m, _ := json.Marshal(manifest)
	request := map[string]any{"id": "api-task", "deployment_id": "api-deployment", "action": "deploy", "definition": map[string]any{"manifest_json": string(m), "version_json": version, "compose_source": compose}}
	prefix := "/v1/admin/deployment-tasks/api-task"
	call("POST", "/v1/admin/deployment-tasks", request, 202)
	call("POST", "/v1/admin/deployment-tasks", request, 202)
	call("POST", prefix+"/claim", map[string]any{"machine_id": "wrong", "hostname": "gpu-host"}, 409)
	claim := call("POST", prefix+"/claim", map[string]any{"machine_id": "i-api", "hostname": "gpu-host"}, 200)
	token := claim["claim_token"].(string)
	task := call("GET", prefix, nil, 200)
	if _, ok := task["claim_token"]; ok {
		t.Fatal("claim token leaked through query")
	}
	event := map[string]any{"claim_token": token, "event": map[string]any{"event_id": "api-event", "deployment_id": "api-deployment", "phase": "staged", "summary": map[string]any{"preserved": true}}}
	call("POST", prefix+"/events", event, 200)
	call("POST", prefix+"/events", event, 200)
	call("POST", prefix+"/heartbeat", map[string]any{"claim_token": "wrong"}, 409)
	call("POST", prefix+"/finish", map[string]any{"claim_token": token, "status": "succeeded", "result": map[string]any{"phase": "staged"}}, 200)
	record := call("GET", "/v1/admin/deployments/api-deployment", nil, 200)
	if record["phase"] != "staged" || len(record["events"].([]any)) != 1 {
		t.Fatal(record)
	}
	call("POST", prefix+"/events", event, 409)

	// Exercise the actual Python executor client over HTTP, not a mirrored mock.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = h.Run() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	}()
	clientDir, err := filepath.Abs("../../deploy/model_deploy")
	if err != nil {
		t.Fatal(err)
	}
	script := `import os,sys,tempfile,pathlib,json
sys.path.insert(0,os.environ['DEPLOY_CLIENT_DIR'])
from control import Control,Audit
from register import Gateway,GatewayError
c=Control(Gateway(os.environ['DEPLOY_TEST_URL'],os.environ['DEPLOY_TEST_TOKEN']))
assert c.ready()['ready']
assert c.history('api-deployment')['phase']=='staged'
t=c.submit('python-stop','stop','api-deployment')
assert t['status']=='queued'
t=c.claim(t['id'],'i-api','gpu-host')
a=Audit(c,t,'api-deployment');a.record('stop_intent');a.record('stopped')
c.heartbeat(t);c.finish(t,'succeeded',{'phase':'stopped'})
assert c.task(t['id'])['status']=='succeeded'
with tempfile.TemporaryDirectory() as root:
 r=c.export('api-deployment',pathlib.Path(root)/'export')
 assert r['phase']=='stopped'
 assert json.loads((pathlib.Path(root)/'export/manifest.json').read_text())['deployment_id']=='api-deployment'
try:
 a.record('active')
 raise AssertionError('finished claim accepted')
except GatewayError as e:
 assert e.status==409
print('PYTHON_HTTP_POSTGRES_PASSED')
`
	command := exec.CommandContext(t.Context(), python, "-c", script)
	command.Env = append(os.Environ(), "DEPLOY_CLIENT_DIR="+clientDir, "DEPLOY_TEST_URL=http://"+l.Addr().String(), "DEPLOY_TEST_TOKEN="+conf.AdminToken)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Python HTTP contract: %v %s", err, out)
	}
	if !strings.Contains(string(out), "PYTHON_HTTP_POSTGRES_PASSED") {
		t.Fatal(string(out))
	}
}
