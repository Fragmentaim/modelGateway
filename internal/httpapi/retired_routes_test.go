package httpapi

import (
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/wgdl666/modelGateway/internal/config"
)

func TestRetiredInferenceRoutesAreNotRegistered(t *testing.T) {
	cfg := config.Config{APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24),
		AllowedNetworks: []string{"172.31.0.0/16"}, AllowedPorts: []string{"8001"},
		VisionAddr: "172.31.43.7:18280", VLMAddr: "172.31.3.219:30332", DecisionAddr: "172.31.3.219:8874"}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := New(nil, runtime, nil).HTTPServer(listener)
	for _, path := range []string{
		"/v1/chat/completions", "/v1/decision/predict",
		"/v1/vision/human-parser/human_parser/predict", "/v1/vision/human-yolo/predict",
		"/v1/vision/facesim/compare", "/v1/vision/segment-person/api/matte",
		"/v1/vision/segment-person/api/segment", "/v1/vision/segment-person/api/segment_subject",
		"/v1/vision/lens_distortion_correction/correct",
	} {
		t.Run(path, func(t *testing.T) {
			got := ut.PerformRequest(server.Engine, http.MethodPost, path, nil, ut.Header{Key: "Authorization", Value: "Bearer " + cfg.APIToken})
			if got.Code != http.StatusNotFound {
				t.Fatalf("retired route returned %d", got.Code)
			}
		})
	}
}
