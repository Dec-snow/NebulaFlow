-- 003_agent_registry.sql
-- Agent 注册中心表
-- 支持多租户、系统内置 Agent、按能力检索

CREATE TABLE IF NOT EXISTS agent_registry (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL DEFAULT 0,         -- 所属用户，0 表示系统内置
    name            VARCHAR(128) NOT NULL,             -- Agent 名称（如 research-assistant）
    description     TEXT NOT NULL DEFAULT '',          -- 人类可读描述
    runtime_type    VARCHAR(32) NOT NULL,              -- native / langchain / http
    endpoint        VARCHAR(512) NOT NULL DEFAULT '',  -- 远程 Agent 地址（native 为空）
    model           VARCHAR(128) NOT NULL DEFAULT '',  -- 默认模型
    capabilities    TEXT[] NOT NULL DEFAULT '{}',      -- 能力标签数组
    status          VARCHAR(16) NOT NULL DEFAULT 'active', -- active / inactive / error
    version         VARCHAR(32) NOT NULL DEFAULT '1.0.0', -- 语义化版本
    timeout_sec     INTEGER NOT NULL DEFAULT 120,      -- 超时秒数
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 索引：用户 + 名称唯一（系统内置 Agent user_id=0 也参与唯一性约束）
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_registry_user_name
    ON agent_registry (user_id, name);

-- 索引：按 Runtime 类型过滤
CREATE INDEX IF NOT EXISTS idx_agent_registry_runtime_type
    ON agent_registry (runtime_type);

-- 索引：按状态过滤
CREATE INDEX IF NOT EXISTS idx_agent_registry_status
    ON agent_registry (status);

-- GIN 索引：按能力检索（capabilities 数组）
CREATE INDEX IF NOT EXISTS idx_agent_registry_capabilities
    ON agent_registry USING GIN (capabilities);
