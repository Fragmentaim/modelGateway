package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/deployment"
)

type deploymentStore interface {
	Ready(context.Context) error
	Submit(context.Context, deployment.Submit) (deployment.Task, error)
	Get(context.Context, string) (deployment.Record, error)
	Task(context.Context, string) (deployment.Task, error)
	Claim(context.Context, string, string, string, bool) (deployment.Task, error)
	Heartbeat(context.Context, string, string) error
	Event(context.Context, string, string, deployment.Event) error
	Finish(context.Context, string, string, string, json.RawMessage) error
}

func (s *Server) SetDeployments(d deploymentStore) { s.deployments = d }
func (s *Server) deploymentHandler(next app.HandlerFunc) app.HandlerFunc {
	return s.auth("admin", func(ctx context.Context, c *app.RequestContext) {
		if !requestConfig(ctx).DeploymentsEnabled || s.deployments == nil {
			writeError(c, 503, "deployments_disabled", "model deployment management is not enabled", "")
			return
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		next(ctx, c)
	})
}
func deploymentError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, deployment.ErrInvalid):
		writeError(c, 400, "invalid_deployment", err.Error(), "")
	case errors.Is(err, deployment.ErrNotFound):
		writeError(c, 404, "not_found", "deployment or task not found", "")
	case errors.Is(err, deployment.ErrConflict):
		writeError(c, 409, "deployment_conflict", "deployment state, ownership or resource conflict", "")
	default:
		writeError(c, 503, "deployment_storage_unavailable", "deployment storage unavailable; retain the current execution and reconcile before retrying", "")
	}
}
func (s *Server) deploymentReady(ctx context.Context, c *app.RequestContext) {
	if err := s.deployments.Ready(ctx); err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 200, map[string]bool{"ready": true})
}
func (s *Server) submitDeployment(ctx context.Context, c *app.RequestContext) {
	var in deployment.Submit
	if _, ok := readBody(c, 4<<20, &in, ""); !ok {
		return
	}
	t, err := s.deployments.Submit(ctx, in)
	if err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 202, t)
}
func (s *Server) getDeployment(ctx context.Context, c *app.RequestContext) {
	r, err := s.deployments.Get(ctx, c.Param("id"))
	if err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 200, r)
}
func (s *Server) deploymentTask(ctx context.Context, c *app.RequestContext) {
	t, err := s.deployments.Task(ctx, c.Param("id"))
	if err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 200, t)
}
func (s *Server) claimDeployment(ctx context.Context, c *app.RequestContext) {
	var in struct {
		Machine  string `json:"machine_id"`
		Hostname string `json:"hostname"`
		Recover  bool   `json:"recover"`
	}
	if _, ok := readBody(c, 4096, &in, ""); !ok {
		return
	}
	t, err := s.deployments.Claim(ctx, c.Param("id"), in.Machine, in.Hostname, in.Recover)
	if err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 200, t)
}
func (s *Server) deploymentHeartbeat(ctx context.Context, c *app.RequestContext) {
	var in struct {
		Token string `json:"claim_token"`
	}
	if _, ok := readBody(c, 4096, &in, ""); !ok {
		return
	}
	if err := s.deployments.Heartbeat(ctx, c.Param("id"), in.Token); err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 200, map[string]bool{"ok": true})
}
func (s *Server) deploymentEvent(ctx context.Context, c *app.RequestContext) {
	var in struct {
		Token string           `json:"claim_token"`
		Event deployment.Event `json:"event"`
	}
	if _, ok := readBody(c, 512<<10, &in, ""); !ok {
		return
	}
	if err := s.deployments.Event(ctx, c.Param("id"), in.Token, in.Event); err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 200, map[string]bool{"ok": true})
}
func (s *Server) finishDeployment(ctx context.Context, c *app.RequestContext) {
	var in struct {
		Token  string          `json:"claim_token"`
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
	}
	if _, ok := readBody(c, 512<<10, &in, ""); !ok {
		return
	}
	if err := s.deployments.Finish(ctx, c.Param("id"), in.Token, in.Status, in.Result); err != nil {
		deploymentError(c, err)
		return
	}
	writeJSON(c, 200, map[string]bool{"ok": true})
}
