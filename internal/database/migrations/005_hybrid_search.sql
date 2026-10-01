-- 005_hybrid_search.sql
-- 为 document_chunks 增加全文检索能力，支持 Hybrid Search（向量 + 关键词）

-- 1. 增加 tsvector 列（中文用简单分词，英文用 english 配置）
ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS content_tsv tsvector;

-- 2. 生成 tsvector（简单分词对中文友好，按空格和标点切分）
--    用 'simple' 配置：不做词干还原，保留原始词形
UPDATE document_chunks
   SET content_tsv = to_tsvector('simple', content)
 WHERE content_tsv IS NULL
   AND content IS NOT NULL
   AND content <> '';

-- 3. GIN 索引：tsvector 全文检索的标准索引
CREATE INDEX IF NOT EXISTS idx_document_chunks_content_tsv
    ON document_chunks USING GIN (content_tsv);

-- 4. 触发器：插入/更新时自动维护 tsvector
--    用触发器而不是生成列，因为生成列 + to_tsvector 在某些 PG 版本有限制
CREATE OR REPLACE FUNCTION document_chunks_tsv_trigger()
RETURNS trigger AS $$
BEGIN
    NEW.content_tsv := to_tsvector('simple', COALESCE(NEW.content, ''));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_document_chunks_tsv ON document_chunks;

CREATE TRIGGER trg_document_chunks_tsv
    BEFORE INSERT OR UPDATE OF content
    ON document_chunks
    FOR EACH ROW
    EXECUTE FUNCTION document_chunks_tsv_trigger();
