package integration

import (
	"os"
	"os/exec"
	"testing"

	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestModelHubAgainstUnifiedGateway(t *testing.T) {
	source := os.Getenv("MODELHUB_CONTRACT_DIR")
	if source == "" {
		t.Skip("MODELHUB_CONTRACT_DIR required")
	}
	ledger, _, db := stores(t)
	for _, model := range []string{protocol.BodyModel, protocol.OutfitModel} {
		registerReady(t, ledger, testInstance(model, "http://127.0.0.1:8001", 1))
	}
	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(ledger, runtime, nil))
	cmd := exec.CommandContext(t.Context(), "go", "test", "-race", "./internal/infra/vworldimage", "-run", "^TestGatewaySchemaContract$", "-count=1", "-v")
	cmd.Dir = source
	cmd.Env = append(os.Environ(), "VMIND_GATEWAY_TEST_URL="+base, "VMIND_GATEWAY_TEST_TOKEN="+apiToken)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ModelHub contract: %v\n%s", err, out)
	}
	t.Logf("ModelHub contract:\n%s", out)
	if count, err := db.Request.Query().Count(t.Context()); err != nil || count != 0 {
		t.Fatalf("invalid business input reached ledger: %d %v", count, err)
	}
}
