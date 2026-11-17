package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/wgdl666/kangaroo/callmeta"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"go.opentelemetry.io/otel/trace"
)

// 从真实领取查询恢复来源，防止只用手工填满的实体测试而漏掉 Select 缺列。
func TestClaimPollPreservesSubmissionMetadata(t *testing.T) {
	for _, withMetadata := range []bool{true, false} {
		name := "legacy_without_metadata"
		if withMetadata {
			name = "with_metadata"
		}
		t.Run(name, func(t *testing.T) {
			a, b, _ := stores(t)
			instance := testInstance(protocol.VideoModel, "http://127.0.0.1:8001", 1)
			registerReady(t, a, instance)
			admission := testAdmission(instance, uuid.NewString(), 1)
			var metadata protocol.Metadata
			if withMetadata {
				span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
				source := callmeta.WithIdentity(trace.ContextWithSpanContext(context.Background(), span), "original-device", "original-user")
				metadata = protocol.CaptureMetadata(source)
				var err error
				admission.Payload, err = json.Marshal(protocol.Submission{Input: admission.Input, Metadata: metadata})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.Admit(t.Context(), admission); err != nil {
				t.Fatal(err)
			}
			task, err := a.Claim(t.Context(), testConfig(), admission.ID)
			if err != nil || task == nil {
				t.Fatalf("claim: %v, %v", task, err)
			}
			if err := a.RecordResponse(t.Context(), task.ID, task.PollClaim, protocol.ModelResponse{Response: protocol.Response{State: "processing"}, BackendTaskID: "video-job"}, 0); err != nil {
				t.Fatal(err)
			}
			poll, err := b.ClaimPoll(t.Context())
			if err != nil || poll == nil || poll.ID != task.ID {
				t.Fatalf("poll: %v, %v", poll, err)
			}
			var saved protocol.Submission
			if err := json.Unmarshal(poll.RequestPayload, &saved); err != nil {
				t.Fatalf("poll omitted saved submission: %v", err)
			}
			if saved.Metadata != metadata {
				t.Fatalf("metadata changed: got %+v, want %+v", saved.Metadata, metadata)
			}
			restored := saved.Metadata.Restore(callmeta.WithIdentity(context.Background(), "query-device", "query-user"))
			device, user := callmeta.Identity(restored)
			if device != metadata.DeviceID || user != metadata.UserID {
				t.Fatalf("restored query identity instead of origin: %q %q", device, user)
			}
			span := trace.SpanContextFromContext(restored)
			if withMetadata && span.TraceID().String() != metadata.TraceID {
				t.Fatal("poll lost original trace")
			}
			if !withMetadata && span.IsValid() {
				t.Fatal("legacy task fabricated trace")
			}
		})
	}
}
