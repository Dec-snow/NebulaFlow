-- 009_documents_index_stats.sql
-- 补齐 documents 表的索引统计列：chunks / size_kb / error_message。
--
-- 背景（真实 PG 16 上复现）：
--   internal/store/knowledge.go 的 ListDocuments / GetDocument /
--   UpdateDocumentIndexResult 一直在读写这三列，但 001_init.sql 建表时
--   documents 只有 id, knowledge_base_id, filename, status, content, created_at。
--   后果不是"少了个统计字段"，而是知识库在 PG 模式下完全不可用：
--     1. GET /documents（列表）与文档详情 500：
--        ERROR: column "chunks" does not exist (SQLSTATE 42703)
--     2. 每个文档索引收尾都要调 UpdateDocumentIndexResult 写状态，同样报错，
--        于是文档永远停在 indexing（rag/indexer.go 已经把分块写进库了）；
--        而 SearchChunks 的 WHERE 带 d.status = 'indexed' 过滤
--        → 检索永远查不到任何东西。
--
-- 为什么内存模式（memory_knowledge.go）一直正常：那里这三个字段本来就在
-- model.Document 上，所以单测全绿、只有 PG 路径烂——这正是"PG 路径零覆盖"的代价。
--
-- 幂等：ADD COLUMN IF NOT EXISTS + NOT NULL DEFAULT，已存在的库重跑无副作用。
-- 与 001 一致地把统计值放在 documents 行上（而不是每次检索去 COUNT 分块），
-- 是为了让 ListDocuments 保持单表查询。

BEGIN;

ALTER TABLE documents ADD COLUMN IF NOT EXISTS chunks        INT  NOT NULL DEFAULT 0;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS size_kb       INT  NOT NULL DEFAULT 0;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS error_message TEXT NOT NULL DEFAULT '';

COMMIT;
