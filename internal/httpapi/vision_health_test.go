package httpapi

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/wgdl666/modelGateway/internal/config"
)

func TestVisionHealth(t *testing.T) {
	cfg := config.Config{
		APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24),
		AllowedNetworks: []string{"10.0.0.0/8"}, AllowedPorts: []string{"8001"},
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, runtime, nil)
	calls := 0
	upstreamBody := `{"status":"degraded","services":{"human-parser":{"ready":true},"human-yolo":{"ready":false},"facesim":{"ready":false},"segment-person":{"ready":true}}}`
	s.direct.Transport = directRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.String() != "http://172.31.43.7:18280/health" ||
			r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected vision health upstream: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(upstreamBody))}, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	h := s.HTTPServer(listener)
	call := func(token string) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, http.MethodGet, "/v1/vision/health?upstream=http://other", nil,
			ut.Header{Key: "Authorization", Value: "Bearer " + token})
	}
	if got := call(cfg.AdminToken); got.Code != 200 || strings.TrimSpace(got.Body.String()) != "[]" || calls != 0 {
		t.Fatalf("unconfigured vision health: %d %s, upstream calls %d", got.Code, got.Body, calls)
	}
	cfg.VisionAddr = "172.31.43.7:18280"
	if err := runtime.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if got := call(cfg.APIToken); got.Code != 401 || calls != 0 {
		t.Fatalf("API token read admin health: %d, upstream calls %d", got.Code, calls)
	}
	got := call(cfg.AdminToken)
	var models []visionModelHealth
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &models) != nil || calls != 1 || len(models) != 6 {
		t.Fatalf("vision health: %d %s, upstream calls %d", got.Code, got.Body, calls)
	}
	// 聚合降级仍应分别展示正常、未就绪和无探测结果，避免一个后端影响其他模型的显示。
	want := map[string]string{
		"facesim": "false", "Vmind_human_parse_v1.0": "true", "Vmind_human_yolo_v1.0": "false", "Vmind_segment_person_bria_v1.0": "true",
		"Vmind_segment_subject_bria_v1.0": "true", "Vmind_lens_distortion_correction_v1.0": "null",
	}
	for _, model := range models {
		ready, _ := json.Marshal(model.Ready)
		if string(ready) != want[model.ModelKey] {
			t.Errorf("model %q readiness: got %s, want %s", model.ModelKey, ready, want[model.ModelKey])
		}
		delete(want, model.ModelKey)
	}
	upstreamBody = `not json`
	got = call(cfg.AdminToken)
	models = nil
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &models) != nil || len(models) != 6 {
		t.Fatalf("malformed upstream should preserve six models: %d %s", got.Code, got.Body)
	}
	for _, model := range models {
		if model.Ready != nil {
			t.Errorf("model %q should have unknown readiness after a failed probe", model.ModelKey)
		}
	}
}

type directRoundTrip func(*http.Request) (*http.Response, error)

func (f directRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
