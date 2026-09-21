-- 002_pgvector.sql
--
-- RAG 检索从「应用层全量扫描 + 余弦」迁移到「数据库向量索引检索」。
--
-- 背景（实测数据见 benchmark/README.md 与优化任务清单 P0-1）：
--   改造前 SearchChunks(kbID) 的 SQL 没有 LIMIT，把整个知识库的分块
--   （含 JSONB 序列化的 embedding）全量拉进进程，再由 rag.Service 逐行
--   json.Unmarshal 成 []float64 并算余弦。1000 文档 × 50 分块时，
--   单次检索的传输与解析量在 300MB 量级，而每次 RAG 节点执行都要重复一遍。
--
-- 本迁移只做两件事：装扩展、加向量列并回填历史数据。
-- 列维度与 HNSW 索引不在 SQL 里做，原因见下方注释。
--
-- 为什么维度不写死在这里：
--   pgvector 的 vector(N) 要求维度固定，而本项目的 Embedder 维度是可配置的
--   （LocalHashEmbedder 默认 384，OpenAI 兼容实现 1536/3072）。
--   SQL 迁移文件无法参数化，所以这里先建「无维度」的 vector 列，
--   由 database.EnsureVectorIndex(dim) 在 Go 侧统一做
--   「校验/修正维度 + 建 HNSW 索引」——那里能拿到配置值。

BEGIN;

-- pgvector 扩展。需要超级用户或该扩展被标记为 trusted。
CREATE EXTENSION IF NOT EXISTS vector;

-- 新增向量列。先不定维度：定维度会让 ALTER 在维度不匹配时直接失败，
-- 而这里希望的是「先把列建出来，再由 Go 侧按配置统一收敛」。
ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS embedding_v vector;

-- 回填历史数据：JSONB 数组 → vector。
--   - 只处理 embedding_v 为空的行，重复执行安全（幂等）；
--   - 跳过空数组：'[]'::vector 是零维向量，无法转换到 vector(N)；
--   - 幂等靠 embedding_v IS NULL，而不是靠 schema_migrations——
--     后者只保证「整个文件只跑一次」，回填逻辑本身仍需能重跑。
UPDATE document_chunks
   SET embedding_v = (embedding)::text::vector
 WHERE embedding_v IS NULL
   AND jsonb_array_length(embedding) > 0;

COMMIT;
