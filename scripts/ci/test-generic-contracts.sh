#!/usr/bin/env bash
# Release gate: unlike optional developer tests, missing dependencies must fail.
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${TEST_DATABASE_URL:?Use an isolated PostgreSQL database ending in _test}"
: "${TEST_RABBITMQ_URL:?Use an isolated test RabbitMQ}"
: "${VMIND_IMAGE_TEST_URL:?Start VMind-Image/tests/generic_gateway_fixture.py first}"
: "${VMIND_IMAGE_MODEL_DEFINITIONS:?Use the fixture .models.json file}"
: "${MODELHUB_CONTRACT_DIR:?Use the integrated wgModelHub checkout}"
test -s "$VMIND_IMAGE_MODEL_DEFINITIONS"
test -f "$MODELHUB_CONTRACT_DIR/internal/infra/vworldimage/gateway_contract_test.go"
go test -race ./internal/integration -run '^(TestOldAndCanonicalImageInputsReplaySameTask|TestLegacyRoutesUseRegisteredTargets|TestMediaAdmissionRejectsMixedModelHubEnvelope|TestVMindImageContract|TestModelHubAgainstUnifiedGateway|TestGenericRoutesUseStoredSchemaBeforeAdmission|TestGenericAndBusinessSharePhysicalGPUCapacity|TestSchemaReplicaComparisonPreservesLargeIntegerConstraints)$' -count=1 -v
