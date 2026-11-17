package integration

import (
	"context"
	"slices"
	"testing"

	entcore "entgo.io/ent"
	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestClaimDoesNotLoadModelSchema(t *testing.T) {
	_, ledger, db := stores(t)
	spec := testInstance(protocol.VideoModel, "http://127.0.0.1:8001", 1)
	registerReady(t, ledger, spec)
	input := testAdmission(spec, uuid.NewString(), 1)
	if _, err := ledger.Admit(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	var projections [][]string
	db.ModelInstance.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(ctx context.Context, query ent.Query) (ent.Value, error) {
			fields := entcore.QueryFromContext(ctx).Fields
			if len(fields) == 0 || slices.Contains(fields, modelinstance.FieldInputSchema) {
				t.Errorf("Claim loaded schema: %v", fields)
			}
			projections = append(projections, slices.Clone(fields))
			return next.Query(ctx, query)
		})
	}))
	claimed, err := ledger.Claim(t.Context(), testConfig(), input.ID)
	if err != nil || claimed == nil || claimed.InstanceID != spec.ID || claimed.ArtifactDigest != spec.Artifact {
		t.Fatalf("claim changed: %+v %v", claimed, err)
	}
	if len(projections) != 2 || len(projections[1]) != 1 || projections[1][0] != modelinstance.FieldID {
		t.Fatalf("unexpected candidate/lock projections: %v", projections)
	}
}
