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

-- 索引：按用户 + 向量相似度检索（HNSW 索引，cosine 距离）
-- 注意：HNSW 索引要求向量维度固定，这里先建空索引，
-- 实际维度由 embedding 模型决定，首次写入后自动生效。
CREATE INDEX IF NOT EXISTS idx_agent_memory_embedding
    ON agent_memory USING hnsw (embedding_v vector_cosine_ops);
