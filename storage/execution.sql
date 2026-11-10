-- Apply explicitly with the application-specific PostgreSQL account; never auto-run at startup.
CREATE TABLE a8s_execution_requests (
  scope TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  request_id TEXT NOT NULL UNIQUE,
  request_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  resource JSONB NOT NULL,
  target JSONB NOT NULL,
  state TEXT NOT NULL,
  result JSONB,
  version BIGINT NOT NULL CHECK (version > 0),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (scope, deployment_id, workspace_id, kind, request_key)
);
CREATE TABLE a8s_execution_threads (
  scope TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  open_request_id TEXT NOT NULL UNIQUE,
  PRIMARY KEY (scope, thread_id)
);
