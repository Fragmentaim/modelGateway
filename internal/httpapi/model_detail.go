package httpapi

import (
	"context"
	"github.com/cloudwego/hertz/pkg/app"
)

func (s *Server) modelDetail(ctx context.Context, c *app.RequestContext) {
	definition, ok := s.resolveModelDefinition(ctx, c, c.Param("model_key"))
	if !ok {
		return
	}
	view, err := modelView(definition, c.GetBool(experimentalCredential), true)
	if err != nil {
		writeError(c, 503, "model_schema_invalid", "invalid model definition", "")
		return
	}
	c.Header("Cache-Control", "no-store")
	writeJSON(c, 200, view)
}
