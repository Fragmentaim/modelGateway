package httpapi

import (
	"context"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/upstreamhealth"
)

type visionModelHealth struct {
	ModelKey string `json:"model_key"`
	Ready    *bool  `json:"ready"`
}

func (s *Server) visionHealth(ctx context.Context, c *app.RequestContext) {
	addr := requestConfig(ctx).VisionAddr
	models := make([]visionModelHealth, 0, len(visionRoutes))
	if addr != "" {
		services := upstreamhealth.ReadVision(ctx, s.direct, "http://"+addr+"/health")
		for _, route := range visionRoutes {
			if route.modelKey == "" {
				continue
			}
			// 两种分割协议共用同一个上游探活结果。
			service := protocol.VisionService(route.protocol)
			models = append(models, visionModelHealth{ModelKey: route.modelKey, Ready: services[service].Ready})
		}
	}
	c.Header("Cache-Control", "no-store")
	writeJSON(c, http.StatusOK, models)
}
