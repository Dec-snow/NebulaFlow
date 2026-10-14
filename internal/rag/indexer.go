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
// 并发安全（重要）：
//   - queue **永远不会被 close**。原实现在 Shutdown 里 `close(idx.queue)`，
//     而 Enqueue 是"先看 stopped、再往队列里投递"，两步之间没有任何互斥，
//     关服瞬间正在上传（HTTP handler 直接调 Enqueue）的请求会命中
//     `panic: send on closed channel`，直接把进程打崩。
//     不再 close 队列 = 从根上消灭 send-on-closed；队列内存交给 GC。
//   - mu 把"检查是否已停止"与"投递"合成一个原子操作：Enqueue 持读锁、
//     Shutdown 持写锁，因此 Shutdown 一旦把 stopped 关掉，后续 Enqueue
//     必然（而不是随机）返回 ErrIndexerStopped。
//
// 关闭时的取舍：Shutdown 之前成功入队的文档会被 worker 排空后处理完
// （见 drain）；只有 Shutdown 超时（ctx 到期）时，剩余未处理文档才会被丢弃
// —— 这一点由调用方通过 ctx 决定，日志里会明确告警。
//
// 状态流转：pending → indexing → indexed / failed
type Indexer struct {
	ragSvc      *Service
	store       store.KnowledgeStore
	concurrency int
	queue       chan *model.Document
	wg          sync.WaitGroup
	logger      *slog.Logger

	// mu 保护 stopped 的"检查 + 投递"与"置停止"这两组操作。
	// Enqueue 走 RLock，Shutdown 走 Lock。
	mu sync.RWMutex
	// stopped 关闭即表示索引器已停止，用于 Enqueue 的快速失败。
	// 只在持有 mu 写锁时关闭（且只关一次）。
	stopped chan struct{}
	// stopOnce 保证停止信号只关闭一次，避免重复 close 导致 panic。
	stopOnce sync.Once
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
// 索引器已停止时返回 ErrIndexerStopped（稳定返回，不会因为并发 Shutdown 而随机变成成功）。
//
// 实现说明：整个"检查 stopped + 投递"都在读锁内完成，与 Shutdown 的写锁互斥。
// 因此不存在"检查通过 → Shutdown 关停 → 投递到已关闭队列"的窗口：
// 要么本次投递在 Shutdown 之前完成（文档随后会被 drain 处理），
// 要么 Shutdown 先拿到写锁，本次调用返回 ErrIndexerStopped。
func (idx *Indexer) Enqueue(doc *model.Document) error {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

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
//
// 注意：这里**不关闭 queue**（关闭它正是 send-on-closed panic 的来源）。
// worker 通过 stopped 信号退出，退出前把队列里已接受的文档排空处理。
func (idx *Indexer) Shutdown(ctx context.Context) error {
	// 写锁 + stopOnce：与 Enqueue 的"检查 + 投递"互斥，
	// 保证停止信号与投递不会交错，也保证 stop() 只关闭一次。
	idx.mu.Lock()
	idx.stopOnce.Do(func() {
		close(idx.stopped)
	})
	idx.mu.Unlock()

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

	for {
		select {
		case doc := <-idx.queue:
			idx.processOne(log, doc)
		case <-idx.stopped:
			// 收到停止信号：把已经"接受"（Enqueue 返回 nil）的文档处理完再退出，
			// 保持"Shutdown 不丢已入队文档"的原有语义。
			idx.drain(log)
			log.Debug("indexer worker exited")
			return
		}
	}
}

// drain 把队列里剩余的文档处理完。
// 队列不会被 close，所以用 non-blocking 接收排空；
// Shutdown 之后不可能再有新文档入队（Enqueue 已改为稳定失败），排空必然终止。
func (idx *Indexer) drain(log *slog.Logger) {
	for {
		select {
		case doc := <-idx.queue:
			idx.processOne(log, doc)
		default:
			return
		}
	}
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
