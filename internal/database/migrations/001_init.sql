-- NebulaFlow 初始 Schema
-- 设计要点：
--   workflows / workflow_nodes / workflow_edges 三表存储 DAG；
--   tasks / task_nodes / task_logs 三表存储执行实例；
--   knowledge_bases / documents / document_chunks 存储 RAG 知识库；
--   llm_providers / llm_models 存储多模型配置；
--   usage_records 记录 token 用量（可观测性 + 计费）。

BEGIN;

-- ================= 用户与认证 =================
CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    username      VARCHAR(64)  NOT NULL UNIQUE,
    email         VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- ================= Workflow =================
CREATE TABLE IF NOT EXISTS workflows (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        VARCHAR(128) NOT NULL,
    description TEXT         NOT NULL DEFAULT '',
    status      VARCHAR(16)  NOT NULL DEFAULT 'draft',  -- draft / published
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workflows_user ON workflows(user_id);

CREATE TABLE IF NOT EXISTS workflow_nodes (
    id          BIGSERIAL PRIMARY KEY,
    workflow_id BIGINT       NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    node_key    VARCHAR(64)  NOT NULL,
    node_type   VARCHAR(16)  NOT NULL,  -- input / llm / rag / tool / output
    config      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    position_x  DOUBLE PRECISION NOT NULL DEFAULT 0,
    position_y  DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (workflow_id, node_key)
);

CREATE TABLE IF NOT EXISTS workflow_edges (
    id          BIGSERIAL PRIMARY KEY,
    workflow_id BIGINT      NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    source_node VARCHAR(64) NOT NULL,
    target_node VARCHAR(64) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workflow_id, source_node, target_node)
);

-- ================= 任务执行 =================
CREATE TABLE IF NOT EXISTS tasks (
    id          BIGSERIAL PRIMARY KEY,
    workflow_id BIGINT      NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status      VARCHAR(16) NOT NULL DEFAULT 'pending',
    input       TEXT        NOT NULL DEFAULT '',
    output      TEXT        NOT NULL DEFAULT '',
    error       TEXT        NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tasks_user   ON tasks(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

CREATE TABLE IF NOT EXISTS task_nodes (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT       NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    node_id     BIGINT       NOT NULL,
    node_key    VARCHAR(64)  NOT NULL,
    node_type   VARCHAR(16)  NOT NULL,
    status      VARCHAR(16)  NOT NULL DEFAULT 'pending',
    input       TEXT         NOT NULL DEFAULT '',
    output      TEXT         NOT NULL DEFAULT '',
    error       TEXT         NOT NULL DEFAULT '',
    retries     INT          NOT NULL DEFAULT 0,
    tokens_in   INT          NOT NULL DEFAULT 0,
    tokens_out  INT          NOT NULL DEFAULT 0,
    duration_ms BIGINT       NOT NULL DEFAULT 0,
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_task_nodes_task ON task_nodes(task_id);

CREATE TABLE IF NOT EXISTS task_logs (
    id         BIGSERIAL PRIMARY KEY,
    task_id    BIGINT      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    node_key   VARCHAR(64) NOT NULL DEFAULT '',
    level      VARCHAR(8)  NOT NULL DEFAULT 'info',
    message    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_logs_task ON task_logs(task_id, id);

-- ================= 知识库 / RAG =================
CREATE TABLE IF NOT EXISTS knowledge_bases (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        VARCHAR(128) NOT NULL,
    description TEXT         NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS documents (
    id                BIGSERIAL PRIMARY KEY,
    knowledge_base_id BIGINT       NOT NULL REFERENCES knowledge_bases(id) ON DELETE CASCADE,
    filename          VARCHAR(255) NOT NULL,
    status            VARCHAR(16)  NOT NULL DEFAULT 'pending',  -- pending / indexed / failed
    content           TEXT         NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS document_chunks (
    id          BIGSERIAL PRIMARY KEY,
    document_id BIGINT       NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    content     TEXT         NOT NULL,
    embedding   JSONB        NOT NULL DEFAULT '[]'::jsonb,  -- float64[]
    chunk_index INT          NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON document_chunks(document_id);

-- ================= LLM Provider / 用量 =================
CREATE TABLE IF NOT EXISTS llm_providers (
    id         BIGSERIAL PRIMARY KEY,
    name       VARCHAR(64)  NOT NULL UNIQUE,   -- deepseek / openai / ollama / mimo ...
    base_url   VARCHAR(255) NOT NULL,
    api_key    VARCHAR(255) NOT NULL DEFAULT '',
    is_default BOOLEAN      NOT NULL DEFAULT false,
    enabled    BOOLEAN      NOT NULL DEFAULT true,
    priority   INT          NOT NULL DEFAULT 100,  -- 越小越优先（fallback 顺序）
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS llm_models (
    id          BIGSERIAL PRIMARY KEY,
    provider_id BIGINT       NOT NULL REFERENCES llm_providers(id) ON DELETE CASCADE,
    name        VARCHAR(128) NOT NULL,
    max_tokens  INT          NOT NULL DEFAULT 4096,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (provider_id, name)
);

CREATE TABLE IF NOT EXISTS usage_records (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    task_id       BIGINT      NOT NULL,
    node_key      VARCHAR(64) NOT NULL DEFAULT '',
    provider      VARCHAR(64) NOT NULL DEFAULT '',
    model         VARCHAR(128) NOT NULL DEFAULT '',
    input_tokens  INT         NOT NULL DEFAULT 0,
    output_tokens INT         NOT NULL DEFAULT 0,
    latency_ms    BIGINT      NOT NULL DEFAULT 0,
    error         TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_usage_user ON usage_records(user_id, created_at DESC);

COMMIT;
