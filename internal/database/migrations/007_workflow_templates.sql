-- 007: 工作流模板字段
-- 为 workflows 表增加模板相关字段，支持模板市场功能

ALTER TABLE workflows
    ADD COLUMN IF NOT EXISTS is_template BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS category    VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS icon        VARCHAR(32) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_workflows_template ON workflows (is_template, category, user_id);
