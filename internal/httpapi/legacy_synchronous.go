package httpapi

import (
	"context"
	"encoding/hex"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"net/http"
	"strings"
)

// Existing fixed routes keep their request ID and trace behavior during the staged rollout.
func (s *Server) forwardLegacySynchronous(ctx context.Context, c *app.RequestContext, client *http.Client, model, kind, path, contentType string, body []byte) {
	id := string(c.GetHeader("Idempotency-Key"))
	if id == "" {
		id = uuid.NewString()
	} else if !protocol.ValidID(id) {
		writeError(c, 400, "invalid_request_id", "Idempotency-Key must be a UUID", "")
		return
	}
	metadata, err := freezeMetadata(ctx, c, protocol.Metadata{TraceID: synchronousTraceID(c)})
	if err != nil {
		writeError(c, 400, "invalid_metadata", err.Error(), id)
		return
	}
	c.Request.Header.Set("Idempotency-Key", id)
	s.forwardSynchronous(ctx, c, client, model, kind, path, contentType, body, metadata)
}
func synchronousTraceID(c *app.RequestContext) string {
	if explicit := string(c.GetHeader("X-Trace-ID")); explicit != "" {
		return explicit
	}
	parts := strings.Split(string(c.GetHeader("traceparent")), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return ""
	}
	// 不把截断、全零或非法的上下文记成有效关联；当前调用链使用 W3C 00 版。
	for _, value := range parts[1:] {
		if _, err := hex.DecodeString(value); err != nil || value != strings.ToLower(value) {
			return ""
		}
	}
	if strings.Trim(parts[1], "0") == "" || strings.Trim(parts[2], "0") == "" {
		return ""
	}
	return parts[1]
}
