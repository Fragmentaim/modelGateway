CREATE SCHEMA IF NOT EXISTS model_deploy;

-- This PR has not been deployed. Reject its earlier draft schema instead of silently reinterpreting data.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='model_deploy'
               AND table_name='deployments' AND column_name='release_sha256') THEN
        RAISE EXCEPTION 'earlier draft schema detected; export records and migrate explicitly before using this schema';
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS model_deploy.versions (
    sha256 text PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    definition jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS model_deploy.deployments (
    id text PRIMARY KEY,
    manifest_sha256 text NOT NULL,
    version_sha256 text NOT NULL REFERENCES model_deploy.versions(sha256),
    model_key text NOT NULL,
    machine_id text NOT NULL,
    gpu_device integer NOT NULL,
    gpu_uuid text NOT NULL,
    manifest jsonb NOT NULL,
    compose_source text NOT NULL,
    predecessor_id text REFERENCES model_deploy.deployments(id),
    phase text NOT NULL DEFAULT 'planned',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- Candidate records may coexist with their predecessor; only one current version may be active.
CREATE UNIQUE INDEX IF NOT EXISTS one_active_model ON model_deploy.deployments(model_key) WHERE phase='active';
CREATE UNIQUE INDEX IF NOT EXISTS one_active_gpu ON model_deploy.deployments(gpu_uuid) WHERE phase='active';
CREATE TABLE IF NOT EXISTS model_deploy.events (
    sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id text NOT NULL REFERENCES model_deploy.deployments(id),
    phase text NOT NULL,
    summary jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deployment_events ON model_deploy.events(deployment_id, sequence);

CREATE OR REPLACE FUNCTION model_deploy.reject_definition_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'versions' THEN
        RAISE EXCEPTION 'model versions are immutable';
    END IF;
    IF (to_jsonb(NEW) - 'phase' - 'updated_at') IS DISTINCT FROM
       (to_jsonb(OLD) - 'phase' - 'updated_at') THEN
        RAISE EXCEPTION 'deployment definitions are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE OR REPLACE TRIGGER immutable_version BEFORE UPDATE ON model_deploy.versions
    FOR EACH ROW EXECUTE FUNCTION model_deploy.reject_definition_change();
CREATE OR REPLACE TRIGGER immutable_deployment BEFORE UPDATE ON model_deploy.deployments
    FOR EACH ROW EXECUTE FUNCTION model_deploy.reject_definition_change();

-- Durable operation ownership survives API/runner restarts. No timed-out job is requeued automatically.
CREATE TABLE IF NOT EXISTS model_deploy.tasks (
 id text PRIMARY KEY,
 deployment_id text NOT NULL REFERENCES model_deploy.deployments(id),
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
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS model_deploy.operation_locks (
 resource_key text PRIMARY KEY,
 task_id text NOT NULL REFERENCES model_deploy.tasks(id)
);
ALTER TABLE model_deploy.events ADD COLUMN IF NOT EXISTS task_id text REFERENCES model_deploy.tasks(id);
ALTER TABLE model_deploy.events ADD COLUMN IF NOT EXISTS event_id text;
CREATE UNIQUE INDEX IF NOT EXISTS deployment_event_idempotency ON model_deploy.events(task_id,event_id) WHERE event_id IS NOT NULL;
