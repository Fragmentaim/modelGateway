package httpapi

import (
	"context"
	"github.com/cloudwego/hertz/pkg/app"
)

func (s *Server) resolveModelProtocol(ctx context.Context, c *app.RequestContext, model string) (string, bool) {
	definition, ok := s.resolveModelDefinition(ctx, c, model)
	return definition.Protocol, ok
}
