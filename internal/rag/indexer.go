package rag

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"unicode/utf8"

	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

var (
	// ErrIndexQueueFull 索引队列已满，新任务无法入队。
	ErrIndexQueueFull = errors.New("index queue is full")
	// ErrIndexerStopped 索引器已停止，不再接受新任务。
	ErrIndexerStopped = errors.New("indexer has been stopped")
)

// DefaultIndexConcurrency 是默认的索引并发度。
// 索引是 CPU + IO 密集型（向量化调用外部 API），并发不宜过高，
// 避免把 embedding 服务打满或占用过多数据库连接。
const DefaultIndexConcurrency = 2

// DefaultIndexQueueSize 是索引队列的默认缓冲大小。
// 队列满时 Enqueue 会返回错误，让调用方（API 层）有机会告知用户"系统繁忙"。
const DefaultIndexQueueSize = 128

// Indexer 是异步文档索引器。
//
// 设计目标：
//   - 上传接口立即返回，索引在后台执行，避免大文件导致 HTTP 超时；
//   - 带缓冲 channel 作为队列，限制并发度，保护下游（embedding 服务、数据库）；
//   - 支持优雅关闭：Shutdown 等待在途任务完成，不丢失已入队的文档。
//
// 状态流转：pending → indexing → indexed / failed
type Indexer struct {
	ragSvc      *Service
	store       store.KnowledgeStore
	concurrency int
	queue       chan *model.Document
	wg          sync.WaitGroup
	logger      *slog.Logger

	// closeOnce 保证关闭通道只执行一次，避免重复 close 导致 panic。
	closeOnce sync.Once
	// stopped 标记索引器已停止，用于 Enqueue 的快速失败。
	stopped chan struct{}
}

// NewIndexer 构造异步索引器。
// concurrency <= 0 时使用 DefaultIndexConcurrency；
// queueSize <= 0 时使用 DefaultIndexQueueSize。
func NewIndexer(ragSvc *Service, ks store.KnowledgeStore, concurrency, queueSize int, logger *slog.Logger) *Indexer {
	if concurrency <= 0 {
		concurrency = DefaultIndexConcurrency
	}
	if queueSize <= 0 {
		queueSize = DefaultIndexQueueSize
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Indexer{
		ragSvc:      ragSvc,
		store:       ks,
		concurrency: concurrency,
		queue:       make(chan *model.Document, queueSize),
		logger:      logger,
		stopped:     make(chan struct{}),
	}
}

// Start 启动索引 worker。应在服务启动阶段调用一次。
func (idx *Indexer) Start() {
	for i := 0; i < idx.concurrency; i++ {
		idx.wg.Add(1)
		go idx.worker(i)
	}
	idx.logger.Info("doc indexer started", "concurrency", idx.concurrency, "queue_size", cap(idx.queue))
}

// Enqueue 将文档提交到索引队列。
// 非阻塞：队列已满时立即返回错误，调用方应向用户返回"系统繁忙，请稍后重试"。
// 索引器已停止时也返回错误。
func (idx *Indexer) Enqueue(doc *model.Document) error {
	select {
	case <-idx.stopped:
		return ErrIndexerStopped
	default:
	}
	select {
	case idx.queue <- doc:
		return nil
	default:
		return ErrIndexQueueFull
	}
}

// Shutdown 优雅关闭索引器。
// 停止接收新任务，等待所有在途（正在执行 + 队列中排队）的任务完成后返回。
// ctx 用于设置等待超时：超时则直接返回，剩余任务将被丢弃。
func (idx *Indexer) Shutdown(ctx context.Context) error {
	idx.closeOnce.Do(func() {
		close(idx.stopped)
		close(idx.queue)
	})
	idx.logger.Info("doc indexer shutting down, waiting for in-flight tasks ...")

	done := make(chan struct{})
	go func() {
		idx.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		idx.logger.Info("doc indexer stopped cleanly")
		return nil
	case <-ctx.Done():
		idx.logger.Warn("doc indexer shutdown timed out, some documents may not be indexed",
			"error", ctx.Err())
		return ctx.Err()
	}
}

// worker 是单个索引协程的主循环。
func (idx *Indexer) worker(id int) {
	defer idx.wg.Done()
	log := idx.logger.With("worker", id)

	for doc := range idx.queue {
		idx.processOne(log, doc)
	}
	log.Debug("indexer worker exited")
}

// processOne 处理单个文档的完整索引流程。
// 任何一步失败都将文档状态置为 failed 并记录错误信息，
// 保证前端能看到失败原因，而不是文档永远停在 indexing。
func (idx *Indexer) processOne(log *slog.Logger, doc *model.Document) {
	log = log.With("doc_id", doc.ID, "filename", doc.Filename)

	// 1. 标记索引开始
	ctx := context.Background()
	if err := idx.store.UpdateDocumentStatus(ctx, doc.ID, model.DocIndexing); err != nil {
		log.Error("failed to set doc status=indexing", "error", err)
		return
	}
	log.Debug("document indexing started")

	// 2. 执行分块 + 向量化
	chunks, err := idx.ragSvc.IndexDocument(ctx, doc)
	if err != nil {
		log.Error("document indexing failed", "error", err)
		if uerr := idx.store.UpdateDocumentIndexResult(ctx, doc.ID,
			model.DocFailed, 0, 0, err.Error()); uerr != nil {
			log.Error("failed to set doc status=failed", "error", uerr)
		}
		return
	}

	// 3. 写入分块（先删旧的，再插新的，保证幂等）
	if err := idx.store.DeleteChunksByDocument(ctx, doc.ID); err != nil {
		log.Error("failed to delete old chunks", "error", err)
		if uerr := idx.store.UpdateDocumentIndexResult(ctx, doc.ID,
			model.DocFailed, 0, 0, "delete old chunks: "+err.Error()); uerr != nil {
			log.Error("failed to set doc status=failed", "error", uerr)
		}
		return
	}
	if len(chunks) > 0 {
		if err := idx.store.BatchInsertChunks(ctx, chunks); err != nil {
			log.Error("failed to insert chunks", "error", err)
			if uerr := idx.store.UpdateDocumentIndexResult(ctx, doc.ID,
				model.DocFailed, 0, 0, "insert chunks: "+err.Error()); uerr != nil {
				log.Error("failed to set doc status=failed", "error", uerr)
			}
			return
		}
	}

	// 4. 标记索引完成，写入统计信息
	sizeKb := (utf8.RuneCountInString(doc.Content) + 1023) / 1024
	if err := idx.store.UpdateDocumentIndexResult(ctx, doc.ID,
		model.DocIndexed, len(chunks), sizeKb, ""); err != nil {
		log.Error("failed to set doc status=indexed", "error", err)
		return
	}
	log.Info("document indexed", "chunks", len(chunks), "size_kb", sizeKb)
}
