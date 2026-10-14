-- 004_agent_memory.sql
-- Agent 长期记忆表
-- 支持按用户隔离、分类检索、pgvector 语义相似度检索

CREATE TABLE IF NOT EXISTS agent_memory (
    id            VARCHAR(64) PRIMARY KEY,           -- 记忆 ID（UUID 或自定义）
    user_id       BIGINT NOT NULL DEFAULT 0,         -- 所属用户
    content       TEXT NOT NULL,                     -- 记忆内容（文本）
    category      VARCHAR(32) NOT NULL DEFAULT 'fact', -- 分类：preference / fact / task_summary
    source        VARCHAR(32) NOT NULL DEFAULT 'conversation', -- 来源
    embedding_v   vector,                            -- 向量（pgvector，无维度，运行时确定）
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb, -- 附加元数据
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 索引：按用户 + 分类查询
CREATE INDEX IF NOT EXISTS idx_agent_memory_user_category
    ON agent_memory (user_id, category);

-- 向量维度与 HNSW 索引不在 SQL 里做，原因与 002_pgvector.sql 完全相同：
--   pgvector 的 vector(N) 要求维度固定，而维度来自配置（EMBED_DIM），
--   SQL 迁移文件无法参数化；对「无维度」的 vector 列建 HNSW 索引会直接失败：
--     ERROR: column does not have dimensions (SQLSTATE 22023)
--
-- 缺陷记录：这里早期确实直接建过 HNSW 索引，后果是**全新数据库上迁移跑不完**——
-- 004 失败 → schema_migrations 不记录该版本 → 每次启动重试并再次失败 → 服务起不来。
-- 因为历史环境可能已把 004 记为 applied，这里只移除索引创建、保留建表语句；
-- 索引改由 Go 侧的 database.EnsureVectorSchema(dim) 按 EMBED_DIM 补齐
-- （先 ALTER COLUMN TYPE vector(N) 再按同名 idx_agent_memory_embedding 建索引，
--   两条路径最终一致，且 IF NOT EXISTS 幂等）。
