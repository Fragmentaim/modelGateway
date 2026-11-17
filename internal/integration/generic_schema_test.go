package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func TestGenericAndBusinessSharePhysicalGPUCapacity(t *testing.T) {
	a, b, db := stores(t)
	business := testInstance(protocol.OutfitModel, "http://127.0.0.1:8001", 1)
	generic := testInstance("generic-same-gpu", "http://127.0.0.1:8002", 1)
	generic.Protocol = protocol.ImageGenerationProtocol
	generic.InputSchema = json.RawMessage(`{"type":"object"}`)
	for _, spec := range []protocol.Instance{business, generic} {
		registerReady(t, a, spec)
	}
	// Exercise both directions: neither protocol gets a separate capacity budget.
	for _, order := range [][]protocol.Instance{{business, generic}, {generic, business}} {
		ids := []string{uuid.NewString(), uuid.NewString()}
		for i, spec := range order {
			if _, err := a.Admit(t.Context(), testAdmission(spec, ids[i], 1)); err != nil {
				t.Fatal(err)
			}
		}
		first, err := a.Claim(t.Context(), testConfig(), ids[0])
		if err != nil || first == nil {
			t.Fatalf("first claim: %v %v", first, err)
		}
		if next, err := b.Claim(t.Context(), testConfig(), ids[1]); err != nil || next != nil {
			t.Fatalf("same GPU oversubscribed across protocols: %v %v", next, err)
		}
		if err := a.RecordResponse(t.Context(), first.ID, first.PollClaim, succeeded(first.ID), time.Millisecond); err != nil {
			t.Fatal(err)
		}
		waitForPublicationTime(t, db, ids[1])
		next, err := b.Claim(t.Context(), testConfig(), ids[1])
		if err != nil || next == nil {
			t.Fatalf("capacity not released: %v %v", next, err)
		}
		if err := b.RecordResponse(t.Context(), next.ID, next.PollClaim, succeeded(next.ID), time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSchemaReplicaComparisonPreservesLargeIntegerConstraints(t *testing.T) {
	ledger, _, _ := stores(t)
	spec := testInstance("exact-schema", "http://127.0.0.1:8001", 1)
	spec.Protocol = protocol.ImageGenerationProtocol
	spec.InputSchema = json.RawMessage(`{"type":"object","properties":{"seed":{"const":9007199254740992}}}`)
	registerReady(t, ledger, spec)
	replica := spec
	replica.ID = uuid.NewString()
	replica.Endpoint = "http://127.0.0.1:8002"
	replica.HealthURL = replica.Endpoint + "/health"
	replica.InputSchema = json.RawMessage(`{"type":"object","properties":{"seed":{"const":9007199254740993}}}`)
	if err := ledger.Register(t.Context(), replica, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("different 64-bit constraints accepted: %v", err)
	}
	replica.InputSchema = spec.InputSchema
	if err := ledger.Register(t.Context(), replica, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGenericRoutesUseStoredSchemaBeforeAdmission(t *testing.T) {
	ledger, _, db := stores(t)
	spec := testInstance("schema-generic", "http://127.0.0.1:8001", 1)
	spec.Protocol = protocol.ImageGenerationProtocol
	ensureTestGroup(t, ledger, spec)
	if err := ledger.Register(t.Context(), spec, ""); !errors.Is(err, store.ErrInputSchemaMissing) {
		t.Fatalf("missing schema: %v", err)
	}
	spec.InputSchema = json.RawMessage(`{"type":"object","required":["prompt"],"properties":{"prompt":{"type":"string"},"images":{"type":"array"},"n":{"type":"integer","const":1},"parameters":{"type":"object","properties":{"steps":{"const":8},"seed":{"type":"integer","minimum":0,"maximum":9223372036854775807}},"additionalProperties":false}},"additionalProperties":false}`)
	registerReady(t, ledger, spec)
	replica := spec
	replica.ID = uuid.NewString()
	replica.Endpoint = "http://127.0.0.1:8002"
	replica.HealthURL = replica.Endpoint + "/health"
	replica.InputSchema = nil
	if err := ledger.Register(t.Context(), replica, ""); err != nil {
		t.Fatal(err)
	}
	replica.InputSchema = []byte(`{"type":"object"}`)
	if err := ledger.Register(t.Context(), replica, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("schema drift accepted: %v", err)
	}
	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(ledger, runtime, nil))
	for _, input := range []string{
		`{"prompt":"x","n":2}`, `{"prompt":"x","n":null}`,
		`{"prompt":"x","parameters":{"steps":20}}`, `{"prompt":"x","parameters":{"seed":9223372036854775808}}`,
		`{"prompt":"x","parameters":{"unknown":true}}`,
	} {
		for _, path := range []string{"/v1/images/generations", "/v1/gateway/requests/" + spec.ModelKey} {
			body := `{"model":"` + spec.ModelKey + `",` + input[1:]
			if strings.Contains(path, "gateway") {
				body = `{"input":` + input + `}`
			}
			req, _ := http.NewRequestWithContext(t.Context(), "POST", base+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiToken)
			req.Header.Set("Idempotency-Key", uuid.NewString())
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 400 {
				t.Fatalf("%s %s: %d %s", path, input, res.StatusCode, raw)
			}
		}
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for k, v := range map[string]string{"model": spec.ModelKey, "prompt": "x", "parameters": `{"steps":20}`} {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("image", "ref.png")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("reference"))
	writer.Close()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", base+"/v1/images/edits", &form)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("multipart schema bypass: %d %s", res.StatusCode, raw)
	}
	if count, err := db.Request.Query().Count(t.Context()); err != nil || count != 0 {
		t.Fatalf("invalid inputs reached ledger: %d %v", count, err)
	}
	req, _ = http.NewRequestWithContext(t.Context(), "GET", base+"/v1/models/"+spec.ModelKey, nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var definition struct {
		InputSchema  json.RawMessage `json:"input_schema"`
		ResponseMode string          `json:"response_mode"`
	}
	if err := json.NewDecoder(res.Body).Decode(&definition); err != nil {
		t.Fatal(err)
	}
	if !sameGenerationJSON(definition.InputSchema, spec.InputSchema) || definition.ResponseMode != "task" {
		t.Fatalf("directory differs: %+v", definition)
	}
}
