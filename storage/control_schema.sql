-- Apply explicitly with the a8s database owner. The server never runs DDL.
CREATE TABLE IF NOT EXISTS a8s_control_components (
    tenant_id text NOT NULL,
    project_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('agentc', 'runtime')),
    id text NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (tenant_id, project_id, kind, id)
);

CREATE TABLE IF NOT EXISTS a8s_control_deployments (
    tenant_id text NOT NULL,
    project_id text NOT NULL,
    id text NOT NULL,
    workspace_id text NOT NULL,
    storage_reference text NOT NULL DEFAULT '',
    version bigint NOT NULL CHECK (version > 0),
    pending boolean NOT NULL,
    touched_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    payload jsonb NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    UNIQUE (tenant_id, project_id, workspace_id)
);

-- A storage reference must identify a globally unique backend resource; two
-- logical Workspace IDs must not alias the same writable storage.
CREATE UNIQUE INDEX IF NOT EXISTS a8s_control_storage_owner
    ON a8s_control_deployments (storage_reference) WHERE storage_reference <> '';
CREATE INDEX IF NOT EXISTS a8s_control_pending
    ON a8s_control_deployments (touched_at, tenant_id, project_id, id) WHERE pending;
