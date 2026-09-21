// ragbench —— P0-1 的量化验证工具。
//
// 对比同一份数据上的两条检索路径：
//
//	旧路径（改造前）：SELECT ... WHERE kb_id=$1 全量取回，含 embedding 文本，
//	                 在进程内 json 反序列化 + 逐条算余弦，再自己排序取 Top-K。
//	新路径（改造后）：ORDER BY embedding_v <=> $1 LIMIT k，由 pgvector 的
//	                 HNSW 索引裁剪候选集，只回传 k 行且不传向量。
//
// 两者跑在同一张表、同一批数据、同一批查询向量上，唯一变量是检索策略本身。
//
// 用法：
//
//	# 单租户基线：目标知识库占满整表
//	ragbench -dsn "<dsn>" -chunks 50000 -dim 384 -queries 30 -shards 1
//
//	# 多租户：5 万分块分散到 20 个知识库，目标库只占 5%。
//	# 这组会暴露 HNSW 全局索引 + knowledge_base_id 后置过滤导致的漏召回。
//	# -iterative all 会灌一次数据、把 off / relaxed_order / strict_order 连测一遍。
//	ragbench -dsn "<dsn>" -chunks 50000 -dim 384 -queries 30 -shards 20 -iterative all
//
// 查询向量默认由「目标库中真实分块的向量 + 高斯扰动」构成（见 makePlantedQueries），
// 这样 recall@k 才有解释力；纯随机向量在高维下几乎等距，recall 会退化成噪声。
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type opts struct {
	dsn     string
	chunks  int
	dim     int
	queries int
	k       int
	// shards 把分块分散到多少个知识库。=1 是单租户（目标知识库占满整表），
	// >1 用来回答一个关键问题：HNSW 索引是全局的，而查询要按 knowledge_base_id
	// 过滤，多租户下规划器还会用索引吗？会不会因为过滤后行数不足而漏召回？
	shards int
	// iterative 对应 hnsw.iterative_scan。HNSW 索引扫描默认只吐 ef_search(=40)
	// 行候选，被 knowledge_base_id 过滤掉之后就会不足 k 条。开启迭代扫描后，
	// 索引会继续往外找，直到凑满 k 或触及 hnsw.max_scan_tuples。
	//
	//	off     —— 默认值，多租户下会漏召回
	//	relaxed_order —— 只保证"能找到"，不保证严格按距离有序
	//	strict_order  —— 保证有序，代价是每次都要维护堆序
	iterative string
	// noise 是查询向量的扰动幅度（各分量标准差）。查询向量由「目标库中真实分块的
	// 向量 + 高斯噪声」构成，这样才有一个确定的最近邻，recall@k 才有解释力。
	noise float64
	keep  bool
}

// 允许的迭代扫描模式，白名单校验后才拼进 SET 语句。
// all 是编排值：灌一次数据，把三种模式连起来各测一遍。
var iterativeModes = map[string]bool{
	"off": true, "relaxed_order": true, "strict_order": true, "all": true,
}

// iterativeOrder 是 all 模式下的测量顺序。
var iterativeOrder = []string{"off", "relaxed_order", "strict_order"}

func main() {
	var o opts
	flag.StringVar(&o.dsn, "dsn", "", "PostgreSQL DSN")
	flag.IntVar(&o.chunks, "chunks", 50000, "写入的分块数量")
	flag.IntVar(&o.dim, "dim", 384, "向量维度（须与 EMBED_DIM 一致）")
	flag.IntVar(&o.queries, "queries", 200, "每条路径的测量轮数")
	flag.IntVar(&o.k, "k", 5, "Top-K")
	flag.IntVar(&o.shards, "shards", 1, "分块分散到多少个知识库（多租户场景）")
	flag.StringVar(&o.iterative, "iterative", "off", "hnsw.iterative_scan: off|relaxed_order|strict_order|all")
	flag.Float64Var(&o.noise, "noise", 0.35, "查询向量相对真实分块向量的扰动幅度（各分量标准差）")
	flag.BoolVar(&o.keep, "keep", false, "跑完后保留测试数据（默认清理）")
	flag.Parse()

	if o.dsn == "" {
		fatalf("必须指定 -dsn")
	}
	if !iterativeModes[o.iterative] {
		fatalf("-iterative 只支持 off / relaxed_order / strict_order，收到 %q", o.iterative)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, o.dsn)
	if err != nil {
		fatalf("连接失败: %v", err)
	}
	defer pool.Close()

	// ---------- 1. 准备隔离的测试数据 ----------
	kbID, cleanup, err := seed(ctx, pool, o)
	if err != nil {
		fatalf("写入测试数据: %v", err)
	}
	defer cleanup()

	var total, withVec int64
	var bytesOnDisk string
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(embedding_v), pg_size_pretty(pg_total_relation_size('document_chunks'))
		  FROM document_chunks`).Scan(&total, &withVec, &bytesOnDisk); err != nil {
		fatalf("统计: %v", err)
	}
	var inKB int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM document_chunks c JOIN documents d ON d.id=c.document_id
		 WHERE d.knowledge_base_id=$1`, kbID).Scan(&inKB); err != nil {
		fatalf("统计目标库: %v", err)
	}
	fmt.Printf("\n数据就绪：全表 %d 个分块（%d 个带向量），表体积 %s，维度 %d\n",
		total, withVec, bytesOnDisk, o.dim)
	fmt.Printf("目标知识库 id=%d 含 %d 个分块，占全表 %.2f%%（共 %d 个知识库）\n",
		kbID, inKB, float64(inKB)*100/float64(total), o.shards)

	// ---------- 2. 查询计划：确认索引真的被用上 ----------
	plan, err := explain(ctx, pool, o, kbID)
	if err != nil {
		fatalf("EXPLAIN: %v", err)
	}
	fmt.Println("\n================ 新路径的查询计划（EXPLAIN ANALYZE）================")
	fmt.Println(plan)

	// ---------- 3. 两条路径对比 ----------
	queries, plantedIDs, err := makePlantedQueries(ctx, pool, kbID, o.queries, o.dim, o.noise)
	if err != nil {
		fmt.Printf("\n提示：无法构造「真实向量 + 扰动」查询（%v），回退到纯随机查询。\n"+
			"注意 384 维均匀随机向量之间几乎等距，recall@k 会退化成噪声，只看漏召回即可。\n", err)
		queries = makeQueryVectors(o.queries, o.dim)
		plantedIDs = nil
	}

	modes := iterativeOrder
	if o.iterative != "all" {
		modes = []string{o.iterative}
	}

	// 预热：三种模式是顺序测量的，不预热的话第一个测的模式会独自承担冷缓存成本，
	// 横向比较就不公平（实测首测的 off 模式反而比后测的 relaxed_order 更慢）。
	if warm := len(queries) / 6; warm > 0 {
		if _, err := runOldPath(ctx, pool, o, kbID, queries[:warm]); err != nil {
			fatalf("预热旧路径: %v", err)
		}
		for _, m := range modes {
			if _, err := runNewPath(ctx, pool, o, kbID, queries[:warm], m); err != nil {
				fatalf("预热新路径(%s): %v", m, err)
			}
		}
	}

	oldRes, err := runOldPath(ctx, pool, o, kbID, queries)
	if err != nil {
		fatalf("旧路径: %v", err)
	}

	newRes := make([]result, 0, len(modes))
	for _, m := range modes {
		r, err := runNewPath(ctx, pool, o, kbID, queries, m)
		if err != nil {
			fatalf("新路径(%s): %v", m, err)
		}
		newRes = append(newRes, r)
	}

	printComparison(oldRes, newRes, o, plantedIDs)
}

// plantedHit 统计「植入的相关分块」是否被召回，以及是否排在第一位。
// 这是本基准里最能反映检索质量的两个数：该找到的有没有找到。
func plantedHit(planted []int64, topIDs [][]int64) (hit, top1, total int) {
	for i := range planted {
		if i >= len(topIDs) {
			break
		}
		total++
		for rank, id := range topIDs[i] {
			if id == planted[i] {
				hit++
				if rank == 0 {
					top1++
				}
				break
			}
		}
	}
	return hit, top1, total
}

// ---------- 数据准备 ----------

// seed 建 shards 个独立知识库 + 文档，把 chunks 轮转分配进去，
// 返回其中一个作为查询目标。返回的 cleanup 会删掉本次创建的全部知识库。
//
// 批量写入走「COPY 到临时表 → 转型插入」两段式：
// 50k × 384 维如果拼成多行 INSERT，SQL 文本会有几百 MB，
// 而 pgx 的 CopyFrom 又不便直接映射 vector 类型，所以用临时 text 列中转。
func seed(ctx context.Context, pool *pgxpool.Pool, o opts) (int64, func(), error) {
	shards := o.shards
	if shards < 1 {
		shards = 1
	}

	var userID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&userID); err != nil {
		return 0, func() {}, fmt.Errorf("需要至少一个用户（先跑一次服务端 seed）: %w", err)
	}

	kbIDs := make([]int64, 0, shards)
	docIDs := make([]int64, 0, shards)
	for i := 0; i < shards; i++ {
		var kbID, docID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO knowledge_bases (user_id, name, description) VALUES ($1,$2,$3) RETURNING id`,
			userID, fmt.Sprintf("ragbench-%d", i), "P0-1 量化验证").Scan(&kbID); err != nil {
			return 0, func() {}, err
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO documents (knowledge_base_id, filename, status, content)
			 VALUES ($1,$2,'indexed','ragbench') RETURNING id`,
			kbID, "ragbench.md").Scan(&docID); err != nil {
			return 0, func() {}, err
		}
		kbIDs = append(kbIDs, kbID)
		docIDs = append(docIDs, docID)
	}

	cleanup := func() {
		if o.keep {
			fmt.Printf("\n测试数据已保留：knowledge_base_ids=%v\n", kbIDs)
			return
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM knowledge_bases WHERE id = ANY($1)`, kbIDs)
		fmt.Println("\n测试数据已清理")
	}

	// 用可重复的伪随机向量，保证两条路径面对完全相同的分布
	rng := rand.New(rand.NewSource(20260919))

	if _, err := pool.Exec(ctx,
		`CREATE TEMP TABLE _bench_vec (document_id bigint, content text, chunk_index int, vec text)`); err != nil {
		return 0, cleanup, err
	}

	const batch = 2000
	rows := make([][]any, 0, batch)
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"_bench_vec"},
			[]string{"document_id", "content", "chunk_index", "vec"},
			pgx.CopyFromRows(rows))
		rows = rows[:0]
		return err
	}

	for i := 0; i < o.chunks; i++ {
		rows = append(rows, []any{
			docIDs[i%shards],
			"分块内容 " + strconv.Itoa(i) + "：用于 P0-1 检索压测的占位文本。",
			i,
			randVecLiteral(rng, o.dim),
		})
		if len(rows) >= batch {
			if err := flush(); err != nil {
				return 0, cleanup, err
			}
		}
	}
	if err := flush(); err != nil {
		return 0, cleanup, err
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO document_chunks (document_id, content, chunk_index, embedding_v)
		 SELECT document_id, content, chunk_index, vec::vector FROM _bench_vec`); err != nil {
		return 0, cleanup, err
	}

	// ANALYZE 是必须的：HNSW 是索引扫描，但外层 JOIN/过滤的代价估算依赖统计信息，
	// 没有统计信息时规划器可能保守地选择顺序扫描。
	if _, err := pool.Exec(ctx, `ANALYZE document_chunks`); err != nil {
		return 0, cleanup, err
	}
	if _, err := pool.Exec(ctx, `ANALYZE documents`); err != nil {
		return 0, cleanup, err
	}
	return kbIDs[0], cleanup, nil
}

func randVecLiteral(rng *rand.Rand, dim int) string {
	var sb strings.Builder
	sb.Grow(dim * 10)
	sb.WriteByte('[')
	for i := 0; i < dim; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(rng.NormFloat64(), 'g', -1, 64))
	}
	sb.WriteByte(']')
	return sb.String()
}

// makePlantedQueries 造一批「贴近真实语料」的查询向量：
// 从目标知识库里挑 n 个分块，拿它们的向量加一点高斯噪声当查询，
// 同时返回被选中的分块 id —— 它们就是"标准答案"。
//
// 为什么不用纯随机向量：384 维均匀随机向量之间几乎等距（维数灾难的集中现象），
// 「最近邻」本身就没有定义——实测纯随机查询下 recall@k 只有 9%~20%，而且不同
// 检索模式之间的差异完全淹没在噪声里。用「真实向量 + 扰动」才有一个确定的最近邻。
//
// 噪声幅度 noise 是各分量的标准差。对分量 ~N(0,1) 的向量，加噪后的余弦相似度
// 约为 1/sqrt(1+noise²)：noise=0.35 时约 0.94，与随机分块（≈0）拉开了明显差距，
// 因此"植入分块是否被召回"就是一个干净、无歧义的质量指标。
//
// 之所以还要单独看"植入命中率"而不是只看 recall@k：真实 Top-5 里除了植入的那一个，
// 其余 4 个在几千个近等距向量里本质上是任意的，HNSW 的近似排序与精确排序不可能对齐，
// recall@k 会被这部分噪声压低。植入命中率才是能反映"该找到的有没有找到"的指标。
func makePlantedQueries(ctx context.Context, pool *pgxpool.Pool, kbID int64, n, dim int, noise float64) ([][]float64, []int64, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.id, c.embedding_v::text
		  FROM document_chunks c JOIN documents d ON d.id = c.document_id
		 WHERE d.knowledge_base_id = $1 AND c.embedding_v IS NOT NULL
		 ORDER BY c.id LIMIT $2`, kbID, n)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	rng := rand.New(rand.NewSource(42))
	out := make([][]float64, 0, n)
	planted := make([]int64, 0, n)
	for rows.Next() {
		var id int64
		var vecText string
		if err := rows.Scan(&id, &vecText); err != nil {
			return nil, nil, err
		}
		base, err := parseVectorLiteral(vecText)
		if err != nil {
			return nil, nil, err
		}
		if len(base) != dim {
			continue
		}
		qv := make([]float64, dim)
		for i := range qv {
			qv[i] = base[i] + noise*rng.NormFloat64()
		}
		out = append(out, qv)
		planted = append(planted, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) < n {
		return nil, nil, fmt.Errorf("目标知识库只有 %d 个可用分块，凑不满 %d 条查询向量", len(out), n)
	}
	return out[:n], planted[:n], nil
}

func makeQueryVectors(n, dim int) [][]float64 {
	rng := rand.New(rand.NewSource(42))
	out := make([][]float64, n)
	for i := range out {
		v := make([]float64, dim)
		for j := range v {
			v[j] = rng.NormFloat64()
		}
		out[i] = v
	}
	return out
}

// ---------- 两条路径 ----------

type result struct {
	name     string
	perQuery time.Duration
	total    time.Duration
	// rowsAvg / bytesAvg 是所有查询的平均值——不能用"最后一次"代替，
	// 多租户下每次返回的行数会不一样。
	rowsAvg  float64
	bytesAvg int64
	// topIDs 记录每次查询实际返回的 id 集合，用来算 recall@k。
	// 旧路径是精确全量排序，它的结果就是 ground truth。
	topIDs [][]int64
	// shortfalls 记录「返回行数不足 k」的查询次数。
	// 多租户场景下这是 HNSW + 后置过滤的典型症状：索引全局扫描只吐 ef_search(=40)
	// 行候选，其中大部分属于别的知识库、被过滤掉，导致凑不满 k 条。
	shortfalls int
}

// recallAtK 用旧路径（精确全量排序）的结果当基准，衡量新路径的召回率。
// 返回 (平均召回率, 有基准可比的查询数)。
func recallAtK(exact, approx [][]int64) (float64, int) {
	var hit, total, n int
	for i := range exact {
		if i >= len(approx) || len(exact[i]) == 0 {
			continue
		}
		set := make(map[int64]bool, len(approx[i]))
		for _, id := range approx[i] {
			set[id] = true
		}
		for _, id := range exact[i] {
			if set[id] {
				hit++
			}
			total++
		}
		n++
	}
	if total == 0 {
		return 0, 0
	}
	return float64(hit) / float64(total), n
}

// explain 打印新路径的查询计划，用来证明走的是 HNSW 索引而不是顺序扫描。
func explain(ctx context.Context, pool *pgxpool.Pool, o opts, kbID int64) (string, error) {
	var probe string
	if err := pool.QueryRow(ctx,
		`SELECT embedding_v::text FROM document_chunks c JOIN documents d ON d.id=c.document_id
		  WHERE d.knowledge_base_id=$1 LIMIT 1`, kbID).Scan(&probe); err != nil {
		return "", err
	}
	rows, err := pool.Query(ctx, `
		EXPLAIN (ANALYZE, BUFFERS)
		SELECT c.id FROM document_chunks c JOIN documents d ON d.id=c.document_id
		 WHERE d.knowledge_base_id=$1 AND d.status='indexed' AND c.embedding_v IS NOT NULL
		 ORDER BY c.embedding_v <=> $2::vector LIMIT $3`, kbID, probe, o.k)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		lines = append(lines, s)
	}
	return strings.Join(lines, "\n"), rows.Err()
}

// runOldPath 复刻改造前的行为：全量取回（含向量文本），进程内算余弦再排序。
// 它是精确的，所以它的 Top-K 同时充当 recall@k 的 ground truth。
func runOldPath(ctx context.Context, pool *pgxpool.Pool, o opts, kbID int64, queries [][]float64) (result, error) {
	res := result{name: "旧路径（全量加载 + 应用层余弦）"}
	var rowsTotal, bytesTotal int64
	start := time.Now()
	for _, qv := range queries {
		rows, err := pool.Query(ctx, `
			SELECT c.id, c.content, c.embedding_v::text
			  FROM document_chunks c JOIN documents d ON d.id=c.document_id
			 WHERE d.knowledge_base_id=$1 AND d.status='indexed'`, kbID)
		if err != nil {
			return res, err
		}
		type cand struct {
			id    int64
			score float64
		}
		var cands []cand
		var bytes int64
		for rows.Next() {
			var id int64
			var content, vecText string
			if err := rows.Scan(&id, &content, &vecText); err != nil {
				rows.Close()
				return res, err
			}
			bytes += int64(len(vecText) + len(content))
			// 这一步就是改造前每次检索都要付的代价：把 JSON 数组解析成 float64
			vec, err := parseVectorLiteral(vecText)
			if err != nil {
				rows.Close()
				return res, err
			}
			cands = append(cands, cand{id, cosine(qv, vec)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
		if len(cands) > o.k {
			cands = cands[:o.k]
		}
		ids := make([]int64, 0, len(cands))
		for _, c := range cands {
			ids = append(ids, c.id)
		}
		res.topIDs = append(res.topIDs, ids)
		rowsTotal += int64(len(cands))
		bytesTotal += bytes
		if len(cands) < o.k {
			res.shortfalls++
		}
	}
	res.total = time.Since(start)
	n := int64(len(queries))
	res.perQuery = res.total / time.Duration(len(queries))
	res.rowsAvg = float64(rowsTotal) / float64(n)
	res.bytesAvg = bytesTotal / n
	return res, nil
}

// runNewPath 是改造后的行为：HNSW 索引裁剪，只回传 k 行。
//
// 每次查询都走一个显式事务，因为 hnsw.iterative_scan 要用 SET LOCAL 设置
// （会话级 SET 会在连接池里泄漏到其他查询上）。三种模式都走事务，
// 保证唯一的变量是 iterative_scan 本身。
func runNewPath(ctx context.Context, pool *pgxpool.Pool, o opts, kbID int64, queries [][]float64, mode string) (result, error) {
	name := "新路径（pgvector HNSW 索引）"
	if mode != "off" {
		name += " + iterative_scan=" + mode
	}
	res := result{name: name}
	var rowsTotal, bytesTotal int64
	start := time.Now()
	for _, qv := range queries {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = "+mode); err != nil {
			_ = tx.Rollback(ctx)
			return res, fmt.Errorf("SET LOCAL hnsw.iterative_scan: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.content, 1 - (c.embedding_v <=> $2::vector) AS score
			  FROM document_chunks c JOIN documents d ON d.id=c.document_id
			 WHERE d.knowledge_base_id=$1 AND d.status='indexed' AND c.embedding_v IS NOT NULL
			 ORDER BY c.embedding_v <=> $2::vector LIMIT $3`, kbID, vecLiteral(qv), o.k)
		if err != nil {
			_ = tx.Rollback(ctx)
			return res, err
		}
		var bytes int64
		var ids []int64
		for rows.Next() {
			var id int64
			var content string
			var score float64
			if err := rows.Scan(&id, &content, &score); err != nil {
				rows.Close()
				_ = tx.Rollback(ctx)
				return res, err
			}
			ids = append(ids, id)
			bytes += int64(len(content)) + 16
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			_ = tx.Rollback(ctx)
			return res, err
		}
		if err := tx.Rollback(ctx); err != nil {
			return res, err
		}
		res.topIDs = append(res.topIDs, ids)
		rowsTotal += int64(len(ids))
		bytesTotal += bytes
		if len(ids) < o.k {
			res.shortfalls++
		}
	}
	res.total = time.Since(start)
	n := int64(len(queries))
	res.perQuery = res.total / time.Duration(len(queries))
	res.rowsAvg = float64(rowsTotal) / float64(n)
	res.bytesAvg = bytesTotal / n
	return res, nil
}

// ---------- 向量工具 ----------

func vecLiteral(v []float64) string {
	var sb strings.Builder
	sb.Grow(len(v) * 10)
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	}
	sb.WriteByte(']')
	return sb.String()
}

func parseVectorLiteral(s string) ([]float64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func cosine(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---------- 输出 ----------

func printComparison(oldR result, newRs []result, o opts, planted []int64) {
	fmt.Println("\n================ 检索路径对比 ================")
	fmt.Printf("数据规模: %d 分块 × %d 维, Top-K=%d, 每条路径测量 %d 次, 租户数=%d\n",
		o.chunks, o.dim, o.k, o.queries, o.shards)
	if len(planted) > 0 {
		fmt.Printf("查询向量: 目标库中真实分块向量 + %.2f 标准差扰动（每条查询都有一个确定的相关分块）\n", o.noise)
	} else {
		fmt.Printf("查询向量: 纯随机（注意高维下几乎等距，recall 无解释力）\n")
	}
	fmt.Println()

	hdr := fmt.Sprintf("%-52s %12s %10s %9s %10s %10s %10s %9s %10s",
		"路径", "单次耗时", "总耗时", "平均行数", "回传字节", "不足k次数", "植入召回", "植入Top1", "recall@k")
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)))

	printRow := func(name string, per, total time.Duration, rowsAvg float64, bytesAvg int64,
		shortfalls int, hit, top1, hitTotal int, recall string) {
		plantedCell, top1Cell := "—", "—"
		if hitTotal > 0 {
			plantedCell = fmt.Sprintf("%d/%d", hit, hitTotal)
			top1Cell = fmt.Sprintf("%d/%d", top1, hitTotal)
		}
		fmt.Printf("%-52s %12s %10s %9.1f %10s %10d %10s %9s %10s\n",
			name,
			per.Round(time.Microsecond).String(),
			total.Round(time.Millisecond).String(),
			rowsAvg, humanBytes(bytesAvg), shortfalls, plantedCell, top1Cell, recall)
	}

	hit, top1, hitTotal := plantedHit(planted, oldR.topIDs)
	printRow(oldR.name, oldR.perQuery, oldR.total, oldR.rowsAvg, oldR.bytesAvg, oldR.shortfalls,
		hit, top1, hitTotal, "基准")
	for _, r := range newRs {
		recall, _ := recallAtK(oldR.topIDs, r.topIDs)
		h, t, tot := plantedHit(planted, r.topIDs)
		printRow(r.name, r.perQuery, r.total, r.rowsAvg, r.bytesAvg, r.shortfalls,
			h, t, tot, fmt.Sprintf("%.1f%%", recall*100))
	}
	fmt.Println(strings.Repeat("-", len(hdr)))

	fmt.Printf("\n基准路径（旧路径）单次耗时 %s\n", oldR.perQuery.Round(time.Microsecond).String())
	for _, r := range newRs {
		recall, _ := recallAtK(oldR.topIDs, r.topIDs)
		h, t, tot := plantedHit(planted, r.topIDs)
		hitRate := ""
		if tot > 0 {
			hitRate = fmt.Sprintf("植入召回 %5.1f%%  植入Top1 %5.1f%%  ",
				float64(h)*100/float64(tot), float64(t)*100/float64(tot))
		}
		fmt.Printf("  %-52s 提速 %5.1f×  回传数据量降 %7.0f×  %srecall@%d %5.1f%%  不足k %d/%d\n",
			r.name,
			float64(oldR.perQuery)/float64(r.perQuery),
			float64(oldR.bytesAvg)/float64(max64(r.bytesAvg, 1)),
			hitRate, o.k, recall*100, r.shortfalls, o.queries)
	}

	bad := false
	for _, r := range newRs {
		if r.shortfalls > 0 {
			bad = true
		}
	}
	if bad {
		fmt.Println("\n⚠ 存在「返回行数不足 k」的查询 —— 这是 HNSW 后置过滤导致的漏召回，不是性能问题。")
	}
	fmt.Println("\n注：pgvector 的 <=> 是余弦距离，与逐条余弦排序在浮点意义上等价；")
	fmt.Println("但 HNSW 是近似最近邻（ANN），候选集大小由 hnsw.ef_search 控制（默认 40），")
	fmt.Println("索引扫描默认最多只吐 ef_search 行，被 knowledge_base_id 过滤后就会不足 k 条。")
	fmt.Println("recall@k 是全集合重叠率，会被「近等距向量之间的任意排序」拉低；")
	fmt.Println("植入召回 / 植入Top1 才反映「该找到的相关分块有没有找到」。")
	fmt.Println("==============================================")
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
