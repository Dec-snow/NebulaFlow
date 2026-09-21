// NebulaFlow 压测矩阵编排器。
//
// 对每个 worker 档位重复：重置环境 → 启动服务 → 预热 → 打流 → 停止服务，
// 最后汇总成一张 Workers × (吞吐 / 时延 / 成功率 / 落地率) 的表。
//
// 之所以用 Go 而不是 shell：需要在 Windows / Linux 上都能可靠地
// 起停子进程、轮询就绪、并在失败时拿到子进程日志，shell 在各平台差异太大。
//
// 用法：
//
//	go run ./benchmark/matrix -workers 1,5,10,20 -n 2000 -c 50
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type matrixOpts struct {
	loadgenBin string
	serverBin  string
	outDir     string
	workers    []int
	// consumers 固定消费循环并发度；0 表示跟随 WORKER_COUNT（默认行为）。
	// 单独把它钉死，是为了做"只放开节点并发"与"同时放开任务并发"的对照实验。
	consumers   int
	tag         string
	concurrency int
	requests    int
	warmup      int
	port        string
	redisAddr   string
	dsn         string
	storage     string
	baseURL     string
	readyWait   time.Duration
	env         []string
}

// fileTag 生成档位的文件名后缀。
// 跑"固定消费度"与"跟随 worker 数"两组实验时，没有 tag 会互相覆盖结果。
func (o matrixOpts) fileTag(workers int) string {
	if o.tag == "" {
		return fmt.Sprintf("w%d", workers)
	}
	return fmt.Sprintf("w%d-%s", workers, o.tag)
}

func main() {
	var (
		loadgenBin = flag.String("loadgen", "bin/loadgen.exe", "压测客户端二进制")
		serverBin  = flag.String("server", "bin/nebulaflow.exe", "服务端二进制")
		outDir     = flag.String("out", "benchmark/results", "结果输出目录")
		workersStr = flag.String("workers", "1,5,10,20", "worker 档位，逗号分隔")
		consumers  = flag.Int("consumers", 0, "固定 CONSUMER_COUNT（0 = 跟随 WORKER_COUNT）")
		tag        = flag.String("tag", "", "结果文件名后缀，用于区分不同实验组")
		conc       = flag.Int("c", 50, "每档位并发连接数")
		reqs       = flag.Int("n", 2000, "每档位提交请求数")
		warmup     = flag.Int("warmup", 100, "每档位预热请求数")
		port       = flag.String("port", "8080", "服务端口")
		redisAddr  = flag.String("redis", "127.0.0.1:6379", "Redis 地址")
		dsn        = flag.String("dsn", "", "PostgreSQL DSN（留空则用内存存储）")
		storage    = flag.String("storage", "memory", "STORAGE_MODE")
		readyWait  = flag.Duration("ready", 30*time.Second, "等待服务就绪的上限")
	)
	flag.Parse()

	o := matrixOpts{
		loadgenBin:  *loadgenBin,
		serverBin:   *serverBin,
		outDir:      *outDir,
		consumers:   *consumers,
		tag:         *tag,
		concurrency: *conc,
		requests:    *reqs,
		warmup:      *warmup,
		port:        *port,
		redisAddr:   *redisAddr,
		dsn:         *dsn,
		storage:     *storage,
		baseURL:     "http://127.0.0.1:" + *port,
		readyWait:   *readyWait,
	}
	for _, s := range strings.Split(*workersStr, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			fatalf("非法 worker 档位 %q: %v", s, err)
		}
		o.workers = append(o.workers, n)
	}
	if len(o.workers) == 0 {
		fatalf("没有有效的 worker 档位")
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		fatalf("创建输出目录失败: %v", err)
	}

	var reports []*report
	for _, w := range o.workers {
		rep, err := runOne(o, w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n[workers=%d] 失败: %v\n", w, err)
			// 打印服务端日志尾部，方便定位
			if tail := tailFile(filepath.Join(o.outDir, fmt.Sprintf("server-%s.log", o.fileTag(w))), 25); tail != "" {
				fmt.Fprintf(os.Stderr, "--- server-w%d.log 末尾 ---\n%s\n", w, tail)
			}
			continue
		}
		reports = append(reports, rep)
	}

	if len(reports) == 0 {
		fatalf("所有档位都失败了")
	}
	printMatrix(reports)
	writeSummary(o.outDir, reports)
}

func runOne(o matrixOpts, workers int) (*report, error) {
	fmt.Printf("\n>>> workers=%d 重置环境 ...\n", workers)
	if err := resetEnv(o); err != nil {
		return nil, fmt.Errorf("重置环境: %w", err)
	}

	logPath := filepath.Join(o.outDir, fmt.Sprintf("server-%s.log", o.fileTag(workers)))
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()

	env := append(os.Environ(),
		"APP_ENV=dev",
		"PORT="+o.port,
		"STORAGE_MODE="+o.storage,
		"REDIS_ADDR="+o.redisAddr,
		"WORKER_COUNT="+strconv.Itoa(workers),
		"CONSUMER_COUNT="+strconv.Itoa(o.consumers),
		"USE_REDIS_QUEUE=true",
		// 压测要测系统吞吐，不能让限流阈值成为天花板；限流语义另有单测覆盖
		"RATE_LIMIT_PER_MIN=1000000",
		"TASK_RATE_LIMIT_PER_MIN=1000000",
		"JWT_SECRET=bench-only-secret",
	)
	if o.dsn != "" {
		env = append(env, "DATABASE_URL="+o.dsn)
	}

	cmd := exec.Command(o.serverBin)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动服务: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	fmt.Printf(">>> workers=%d 等待就绪 ...\n", workers)
	if err := waitReady(o.baseURL, o.readyWait); err != nil {
		return nil, err
	}

	jsonOut := filepath.Join(o.outDir, o.fileTag(workers)+".json")
	args := []string{
		"-c", strconv.Itoa(o.concurrency),
		"-n", strconv.Itoa(o.requests),
		"-warmup", strconv.Itoa(o.warmup),
		"-workflow", "1",
		"-workers", strconv.Itoa(workers),
		"-label", fmt.Sprintf("workers=%d consumers=%s", workers, consumersLabel(o.consumers)),
		"-out", jsonOut,
		"-url", o.baseURL,
	}
	if o.dsn != "" {
		args = append(args, "-dsn", o.dsn)
	}

	fmt.Printf(">>> workers=%d 打流中 (c=%d n=%d) ...\n", workers, o.concurrency, o.requests)
	lg := exec.Command(o.loadgenBin, args...)
	lg.Stdout = os.Stdout
	lg.Stderr = os.Stderr
	if err := lg.Run(); err != nil {
		return nil, fmt.Errorf("压测客户端退出异常: %w", err)
	}

	raw, err := os.ReadFile(jsonOut)
	if err != nil {
		return nil, fmt.Errorf("读取压测结果: %w", err)
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("解析压测结果: %w", err)
	}
	// loadgen 无从得知服务端的消费并发度（那是服务端环境变量），由编排器补上，
	// 否则汇总表里"固定消费度"与"跟随 worker 数"两组数据长得一模一样。
	rep.Consumers = o.consumers
	return &rep, nil
}

func consumersLabel(n int) string {
	if n <= 0 {
		return "follow-workers"
	}
	return strconv.Itoa(n)
}

// waitReady 轮询登录接口，直到服务真的能处理请求。
// 只探测端口是不够的：PG 迁移、Redis 握手、worker 启动都发生在监听之后。
func waitReady(baseURL string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	body := []byte(`{"username":"demo","password":"demo123456"}`)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		cli := &http.Client{Timeout: 3 * time.Second}
		resp, err := cli.Do(req)
		if err == nil {
			resp.Body.Close()
			// 内存模式下 seed 数据不存在，登录会 401；能返回任何 HTTP 状态码
			// 就说明路由已就绪，足以开始压测。
			if resp.StatusCode > 0 {
				return nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("等待服务就绪超时 (%v)，最后错误: %v", limit, lastErr)
}

func resetEnv(o matrixOpts) error {
	// Redis：清掉 stream / 消费组 / DLQ / 延迟队列，避免上一轮的残留消息
	if o.redisAddr != "" {
		if err := flushRedis(o.redisAddr); err != nil {
			return fmt.Errorf("清空 redis: %w", err)
		}
	}
	if o.dsn != "" {
		if err := truncateTasks(o.dsn); err != nil {
			return fmt.Errorf("清空任务表: %w", err)
		}
	}
	return nil
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
