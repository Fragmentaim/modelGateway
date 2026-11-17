package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// Old and new ModelHub replicas must replay the same task during a rolling release.
func TestOldAndCanonicalImageInputsReplaySameTask(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	for _, tc := range []struct{ model, canonical, legacy string }{
		{protocol.BodyModel, `{"images":[` + image + `],"gender":"female"}`, `{"images":[` + image + `],"text":"","model_traits":{"gender":"female"}}`},
		{protocol.ClothGenModel, `{"images":[` + image + `],"garment_name":"shirt"}`, `{"images":[` + image + `],"text":"{\"garment_name\":\"shirt\"}"}`},
		{protocol.WardrobeModel, `{"images":[` + image + `],"garment_name":"shirt","bbox":[1,2,3,4],"bbox_format":"pixel_xyxy"}`, `{"images":[` + image + `],"text":"{\"garment_name\":\"shirt\",\"bbox\":[1,2,3,4],\"bbox_format\":\"pixel_xyxy\"}"}`},
		{protocol.OutfitModel, `{"images":[` + image + `,` + image + `],"prompt":" preserve this prompt "}`, `{"images":[` + image + `,` + image + `],"text":" preserve this prompt "}`},
	} {
		t.Run(tc.model, func(t *testing.T) {
			ledger, _, db := stores(t)
			spec := testInstance(tc.model, "http://127.0.0.1:8001", 1)
			registerReady(t, ledger, spec)
			id := uuid.NewString()
			admission := testAdmission(spec, id, 1)
			admission.Input = json.RawMessage(tc.canonical)
			admission.Payload, _ = json.Marshal(protocol.Submission{Input: admission.Input})
			if _, err := ledger.Admit(t.Context(), admission); err != nil {
				t.Fatal(err)
			}
			task, err := ledger.Claim(t.Context(), testConfig(), id)
			if err != nil || task == nil {
				t.Fatalf("claim: %v", err)
			}
			if err := ledger.RecordResponse(t.Context(), id, task.PollClaim, succeeded(id), time.Millisecond); err != nil {
				t.Fatal(err)
			}
			runtime, err := config.NewRuntime(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			base := startHTTPServer(t, httpapi.New(ledger, runtime, nil))
			for _, input := range []string{tc.legacy, tc.canonical} {
				res := sendSynchronous(t, base, "/v1/gateway/requests/"+tc.model, "application/json", `{"input":`+input+`,"output":"image"}`, id)
				raw, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if res.StatusCode != http.StatusOK {
					t.Fatalf("replay: %d %s", res.StatusCode, raw)
				}
				var response protocol.Response
				if err := json.Unmarshal(raw, &response); err != nil || response.RequestID != id || response.State != "succeeded" {
					t.Fatalf("replayed result: %s", raw)
				}
			}
			if n, err := db.Request.Query().Count(t.Context()); err != nil || n != 1 {
				t.Fatalf("duplicate tasks: %d %v", n, err)
			}
		})
	}
}
