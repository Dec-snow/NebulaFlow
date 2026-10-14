package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

type knowledgeHandler struct {
	store   *store.Store
	rag     *rag.Service
	indexer *rag.Indexer
}

// POST /api/knowledge-bases
func (h *knowledgeHandler) createKB(c *gin.Context) {
	u := getUser(c)
	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	kb := &model.KnowledgeBase{UserID: u.ID, Name: req.Name, Description: req.Description}
	if err := h.store.Knowledge.CreateKB(c.Request.Context(), kb); err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusCreated, kb)
}

// GET /api/knowledge-bases
func (h *knowledgeHandler) listKB(c *gin.Context) {
	u := getUser(c)
	list, err := h.store.Knowledge.ListKBs(c.Request.Context(), u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"knowledge_bases": list})
}

// DELETE /api/knowledge-bases/:id
func (h *knowledgeHandler) deleteKB(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	if err := h.store.Knowledge.DeleteKB(c.Request.Context(), id, u.ID); err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"deleted": id})
}

// GET /api/knowledge-bases/:id/documents
func (h *knowledgeHandler) listDocs(c *gin.Context) {
	id := mustID(c)
	u := getUser(c)
	// 越权校验：原实现只看 kbID 不看归属，任何登录用户都能枚举他人知识库文档
	if ok, err := h.store.Knowledge.BelongsToUser(c.Request.Context(), id, u.ID); err != nil {
		abortWithError(c, err)
		return
	} else if !ok {
		abortJSON(c, http.StatusNotFound, gin.H{"error": "knowledge base not found"})
		return
	}
	docs, err := h.store.Knowledge.ListDocuments(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"documents": docs})
}

// POST /api/knowledge-bases/:id/documents
// 上传文本文档（txt/md；PDF 在 API 层抽取文本流）。body 为纯文本或 JSON {content, filename}。
func (h *knowledgeHandler) uploadDoc(c *gin.Context) {
	kbID := mustID(c)
	u := getUser(c)
	ok, err := h.store.Knowledge.BelongsToUser(c.Request.Context(), kbID, u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	if !ok {
		// 原实现在 !ok 且 err==nil 时调用 abortWithError(c, nil)，
		// 内部 err.Error() 直接 panic（gin.Recovery 只返回 500，真实原因丢失）。
		abortJSON(c, http.StatusNotFound, gin.H{"error": "knowledge base not found"})
		return
	}

	contentType := c.GetHeader("Content-Type")
	var content, filename string
	if strings.HasPrefix(contentType, "application/json") {
		var req struct {
			Content  string `json:"content" binding:"required"`
			Filename string `json:"filename"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		content, filename = req.Content, req.Filename
	} else {
		raw, err := c.GetRawData()
		if err != nil {
			abortJSON(c, http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			return
		}
		content = string(raw)
		filename = c.Query("filename")
	}
	if filename == "" {
		filename = "document.txt"
	}
	content = extractPlainText(filename, content)
	if strings.TrimSpace(content) == "" {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": "empty document"})
		return
	}

	doc := &model.Document{
		KnowledgeBaseID: kbID, Filename: filename, Status: model.DocPending, Content: content,
	}
	if err := h.store.Knowledge.CreateDocument(c.Request.Context(), doc); err != nil {
		abortWithError(c, err)
		return
	}
	// 异步索引：提交到后台队列，立即返回 202。
	// 大文件向量化可能耗时较长，同步执行会导致 HTTP 超时；
	// 客户端可通过 GET /documents 轮询或后续 SSE 等待索引完成。
	if h.indexer != nil {
		if err := h.indexer.Enqueue(doc); err != nil {
			// 队列满时文档已创建但无法立即索引，返回 202 并提示稍后重试或自动排队。
			// 这里选择返回成功但带 warning，让用户知道文档已上传、索引会延后。
			writeJSON(c, http.StatusAccepted, gin.H{
				"document": doc,
				"warning":  "document uploaded, but index queue is full; will be retried later",
			})
			return
		}
	} else {
		// 兼容未启用异步索引器的场景（如测试环境），退化为同步索引。
		if err := h.indexDocument(c.Request.Context(), doc); err != nil {
			_ = h.store.Knowledge.UpdateDocumentStatus(c.Request.Context(), doc.ID, model.DocFailed)
			writeJSON(c, http.StatusOK, gin.H{"document": doc, "warning": "index failed: " + err.Error()})
			return
		}
	}
	writeJSON(c, http.StatusCreated, doc)
}

func (h *knowledgeHandler) indexDocument(ctx context.Context, doc *model.Document) error {
	chunks, err := h.rag.IndexDocument(ctx, doc)
	if err != nil {
		return err
	}
	_ = h.store.Knowledge.DeleteChunksByDocument(ctx, doc.ID)
	if len(chunks) > 0 {
		// 批量插入分块，减少 DB round-trip
		if err := h.store.Knowledge.BatchInsertChunks(ctx, chunks); err != nil {
			return err
		}
	}
	return h.store.Knowledge.UpdateDocumentStatus(ctx, doc.ID, model.DocIndexed)
}

// GET /api/knowledge-bases/:id/retrieve?q=...&k=5 —— RAG 检索测试接口
func (h *knowledgeHandler) retrieve(c *gin.Context) {
	kbID := mustID(c)
	u := getUser(c)
	// 归属校验必须区分"不属于你"（404）和"查不出来"（500）。
	//
	// 原实现是 `if ok, _ := ...; !ok` —— 把 err 丢掉了。后果是数据库/连接池故障时，
	// 所有用户看到的都是 404「知识库不存在」：监控上没有任何 5xx，
	// 用户却被告知"你的知识库没了"，排查方向从一开始就完全错了。
	// 这与 auth.Login 里"数据库故障不能被伪装成凭据错误"是同一类缺陷。
	ok, err := h.store.Knowledge.BelongsToUser(c.Request.Context(), kbID, u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	if !ok {
		abortJSON(c, http.StatusNotFound, gin.H{"error": "knowledge base not found"})
		return
	}
	query := c.Query("q")
	if query == "" {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": "missing q param"})
		return
	}
	// 检索下推到存储层：只回传 Top-K，不再全量加载知识库分块
	hits, err := h.rag.Retrieve(c.Request.Context(), h.store.Knowledge, kbID, query, parseQueryInt(c, "k", 5))
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"hits": hits})
}

// extractPlainText 依据扩展名做最小文本抽取。
// 第一版：txt/md 直通；pdf 抽取可打印文本段（无外部 PDF 库时的轻量实现）。
func extractPlainText(filename, content string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".txt", ".md", ".markdown", ".text":
		return content
	case ".pdf":
		return extractPDFText(content)
	default:
		return content
	}
}

// extractPDFText 对 PDF 文本层做非常轻量的抽取：
// 过滤二进制流中的可打印文本。真实产品应使用 pdfium/pdftotext。
// 上传侧（前端）也可先转成文本再上传。
func extractPDFText(raw string) string {
	var sb strings.Builder
	for _, r := range raw {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 32 && r <= 126) {
			sb.WriteRune(r)
		} else if sb.Len() > 0 {
			sb.WriteString(" ")
		}
	}
	return strings.Join(strings.Fields(sb.String()), "\n")
}
