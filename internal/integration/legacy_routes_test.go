package integration

import (
	"fmt"
	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sendLegacySynchronous(t *testing.T, base, path, kind, body, id string, models ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) > 0 {
		req.Header.Set("X-Model-Key", models[0])
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", kind)
	req.Header.Set("Idempotency-Key", id)
	req.Header.Set("traceparent", "00-1234567890abcdef1234567890abcdef-1234567890abcdef-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func TestLegacyRoutesUseRegisteredTargets(t *testing.T) {
	a, _, db := stores(t)
	cfg := testConfig()
	cfg.VisionAddr = "172.31.43.7:18280"
	cfg.DecisionAddr = "172.31.3.219:8874"
	cfg.VLMAddr = "172.31.3.219:30332"
	const multipart = "multipart/form-data; boundary=fixture"
	const form = "--fixture\r\nContent-Disposition: form-data; name=\"image\"\r\n\r\naW1hZ2U=\r\n--fixture--\r\n"
	cases := []struct{ model, port, path, target, kind, body string }{
		{"registered-parser-v2", "6104", "/v1/vision/human-parser/human_parser/predict", "/_dispatch/6104/human-parser/human_parser/predict", "application/json", `{"image":"aW1hZ2U="}`},
		{protocol.HumanParseModel, "6105", "/v1/vision/human-parser/human_parser/predict", "/_dispatch/6105/human-parser/human_parser/predict", "application/json", `{"image":"aW1hZ2U="}`},
		{protocol.HumanYOLOModel, "6102", "/v1/vision/human-yolo/predict", "/_dispatch/6102/human-yolo/predict", multipart, form},
		{protocol.SegmentPersonModel, "6106", "/v1/vision/segment-person/api/segment", "/_dispatch/6106/segment-person/api/segment", multipart, form},
		{protocol.SegmentSubjectModel, "6106", "/v1/vision/segment-person/api/segment_subject", "/_dispatch/6106/segment-person/api/segment_subject", multipart, form},
		{protocol.LensCorrectionModel, "6117", "/v1/vision/lens_distortion_correction/correct", "/_dispatch/6117/lens_distortion_correction/correct", "application/json", `{"base64":"aW1hZ2U=","mirror_lens":false}`},
		{protocol.FaceSimModel, "6103", "/v1/vision/facesim/compare", "/_dispatch/6103/facesim/compare", multipart, strings.Replace(form, `name="image"`, `name="image_a"`, 1)},
		{protocol.DecisionModel, "19101", "/v1/decision/predict", "/v1/instances/19101/predict", multipart, form},
		{protocol.DreamModel, "", "/v1/chat/completions", "/v1/chat/completions", "application/json", `{"model":"Vmind_dream_v1","messages":[{"role":"user","content":"hello"}]}`},
	}
	var expected string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 标准上下文传到模型执行端，业务 Trace ID 和敏感头隔离仍保持原约定。
		if r.URL.Path != expected || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Trace-ID") != "1234567890abcdef1234567890abcdef" ||
			!strings.HasPrefix(r.Header.Get("traceparent"), "00-1234567890abcdef1234567890abcdef-") {
			t.Errorf("wrong execution request: %s", r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			t.Errorf("lost request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"answer":"unchanged"}`)
	}))
	defer upstream.Close()
	base := synchronousServer(t, a, cfg, upstream)
	// 已配置地址而未登记时必须拒绝，不能偷偷继续直连。
	missing := sendLegacySynchronous(t, base, cases[0].path, cases[0].kind, cases[0].body, uuid.NewString())
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unregistered returned %d", missing.StatusCode)
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			spec := testInstance(tc.model, "http://127.0.0.1:"+tc.port, 1)
			if tc.model == protocol.DreamModel {
				spec.Endpoint = "http://" + cfg.VLMAddr
			}
			spec.HealthURL = cfg.SynchronousHealthURL(spec.Protocol)
			if tc.model == protocol.FaceSimModel {
				spec.ResourceGroup = "host/cpu/facesim"
				spec.GPUDevices = nil
				if err := db.ResourceGroup.Create().SetID(spec.ResourceGroup).SetCapacityUnits(1).Exec(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := a.Register(t.Context(), spec, ""); err != nil {
					t.Fatal(err)
				}
				if err := a.RecordProbe(t.Context(), spec.ID, time.Now().UTC(), true); err != nil {
					t.Fatal(err)
				}
			} else {
				registerReady(t, a, spec)
			}
			expected = tc.target
			id := uuid.NewString()
			resp := sendLegacySynchronous(t, base, tc.path+"?target=ignored", tc.kind, tc.body, id, tc.model)
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || string(raw) != `{"answer":"unchanged"}` || resp.Header.Get("X-Model-Instance") != spec.ID {
				t.Fatalf("response: %d %s %v", resp.StatusCode, raw, err)
			}
			row := waitSynchronousState(t, db, id, request.StateSucceeded, false)
			if row.InstanceID != spec.ID || row.Protocol != spec.Protocol || row.ModelKey != spec.ModelKey || row.Endpoint != spec.Endpoint || row.TraceID != "1234567890abcdef1234567890abcdef" || row.QueuePublishAt != nil || strings.Contains(string(row.RequestPayload), "aW1hZ2U=") {
				t.Fatalf("wrong dispatch evidence: %+v", row)
			}
			replay := sendLegacySynchronous(t, base, tc.path, tc.kind, tc.body, id, tc.model)
			replay.Body.Close()
			if replay.StatusCode != 409 {
				t.Fatalf("replay must not execute again: %d", replay.StatusCode)
			}
		})
	}
}
