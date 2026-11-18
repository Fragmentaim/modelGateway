-- Keep deployment management disabled throughout this migration and code rollout.
-- Run as the existing database migration owner, with stop-on-error enabled.
BEGIN;
SELECT pg_advisory_xact_lock(72104982061118);

-- The initial five-table layout has not served a GPU deployment in this rollout.
-- Remove it ONLY when all tables are empty. Never discard or reinterpret history.
DO $$
DECLARE item record; populated boolean;
BEGIN
    FOR item IN SELECT tablename FROM pg_tables WHERE schemaname='model_deploy' ORDER BY tablename LOOP
        IF item.tablename NOT IN ('versions','deployments','tasks','events','operation_locks') THEN
            RAISE EXCEPTION 'unexpected legacy table model_deploy.%; preserve schema and inspect before migration', item.tablename;
        END IF;
        -- Keep the emptiness check valid until DROP, including direct DB writers.
        EXECUTE format('LOCK TABLE model_deploy.%I IN ACCESS EXCLUSIVE MODE', item.tablename);
        EXECUTE format('SELECT EXISTS(SELECT 1 FROM model_deploy.%I)', item.tablename) INTO populated;
        IF populated THEN
            RAISE EXCEPTION 'legacy table model_deploy.% contains data; preserve it and migrate records explicitly before enabling deployment management', item.tablename;
        END IF;
    END LOOP;
END $$;
-- No CASCADE: unexpected dependencies abort the transaction as well.
DROP TABLE IF EXISTS model_deploy.operation_locks;
DROP TABLE IF EXISTS model_deploy.events;
DROP TABLE IF EXISTS model_deploy.tasks;
DROP TABLE IF EXISTS model_deploy.deployments;
DROP TABLE IF EXISTS model_deploy.versions;
DROP FUNCTION IF EXISTS model_deploy.reject_definition_change();
DROP SCHEMA IF EXISTS model_deploy;

CREATE SCHEMA IF NOT EXISTS model_gateway;
CREATE TABLE IF NOT EXISTS model_gateway.model_deployments (
    id text PRIMARY KEY,
    manifest_sha256 text NOT NULL,
    version_sha256 text NOT NULL CHECK (version_sha256 ~ '^[0-9a-f]{64}$'),
    model_key text NOT NULL,
    machine_id text NOT NULL,
    gpu_device integer NOT NULL,
    gpu_uuid text NOT NULL,
    -- Complete immutable version definition is already inside manifest.version.
    manifest jsonb NOT NULL,
    compose_source text NOT NULL,
    predecessor_id text REFERENCES model_gateway.model_deployments(id),
    phase text NOT NULL DEFAULT 'planned',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS model_deployments_one_active_model ON model_gateway.model_deployments(model_key) WHERE phase='active';
CREATE UNIQUE INDEX IF NOT EXISTS model_deployments_one_active_gpu ON model_gateway.model_deployments(gpu_uuid) WHERE phase='active';
CREATE INDEX IF NOT EXISTS model_deployments_predecessor ON model_gateway.model_deployments(predecessor_id);

CREATE OR REPLACE FUNCTION model_gateway.reject_model_deployment_definition_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'phase' - 'updated_at') IS DISTINCT FROM
       (to_jsonb(OLD) - 'phase' - 'updated_at') THEN
        RAISE EXCEPTION 'deployment definitions are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE OR REPLACE TRIGGER immutable_model_deployment BEFORE UPDATE ON model_gateway.model_deployments
    FOR EACH ROW EXECUTE FUNCTION model_gateway.reject_model_deployment_definition_change();

CREATE TABLE IF NOT EXISTS model_gateway.model_deployment_tasks (
    id text PRIMARY KEY,
    deployment_id text NOT NULL REFERENCES model_gateway.model_deployments(id),
    action text NOT NULL CHECK(action IN ('deploy','switch-lora','rollback','stop','adopt')),
    execution_action text NOT NULL,
    activate boolean NOT NULL DEFAULT false,
    status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','succeeded','failed','recovery_required')),
    machine_id text NOT NULL,
    hostname text NOT NULL,
    claim_token text,
    attempt integer NOT NULL DEFAULT 0,
    heartbeat_at timestamptz,
    result jsonb NOT NULL DEFAULT '{}',
    events jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(events)='array'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS model_deployment_tasks_history ON model_gateway.model_deployment_tasks(deployment_id,created_at);
-- Unfinished tasks retain model/GPU ownership across restarts; no lease expiry.
CREATE INDEX IF NOT EXISTS model_deployment_tasks_unfinished ON model_gateway.model_deployment_tasks(deployment_id)
    WHERE status IN ('queued','running','recovery_required');
COMMIT;
