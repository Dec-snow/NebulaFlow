// streamstat —— Redis Stream 容量水位观测工具（P0-2）。
//
// 为什么需要它：P0-2 的验收标准是「压测后 XLEN 稳定在 MaxLen 附近，而不是随提交量持续上涨」，
// 而 queue.Len() 返回的是「积压量」（lag + pending + 延迟重投），不是 stream 的物理长度——
// 两者会同时增长，但只有后者能证明消息本体被真正裁剪掉了。
//
// 用法：
//
//	# 观测当前水位
//	streamstat -addr 127.0.0.1:6379 -stream workflow_tasks
//
//	# 自检：本机 Redis 是否支持 XADD 的 MAXLEN ~ 近似裁剪，以及裁剪是否真的生效
//	streamstat -addr 127.0.0.1:6379 -selftest -maxlen 1000 -probe 20000
//
//	# 清空水位（对照实验前把基线归零；务必在服务端停止时执行）
//	streamstat -addr 127.0.0.1:6379 -reset
//
//	# 压测期间连续采样，一行一个样本（追加到文件，便于 tail -f 看实时曲线）
//	streamstat -addr 127.0.0.1:6379 -watch -interval 500ms -out samples.txt
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "Redis 地址")
	stream := flag.String("stream", "workflow_tasks", "主 stream 名")
	selftest := flag.Bool("selftest", false, "跑近似裁剪自检（会临时写一个 probe key 并删除）")
	reset := flag.Bool("reset", false, "删除主 stream / DLQ / 延迟重投 ZSET（对照实验前归零，需先停服务端）")
	watch := flag.Bool("watch", false, "连续采样（压测期间观察曲线）")
	interval := flag.Duration("interval", 500*time.Millisecond, "watch 采样间隔")
	out := flag.String("out", "", "watch 输出文件（留空写 stdout）")
	maxlen := flag.Int64("maxlen", 1000, "自检使用的 MaxLen")
	probe := flag.Int64("probe", 20000, "自检写入的消息条数")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rdb := redis.NewClient(&redis.Options{Addr: *addr})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		fatalf("连接 %s: %v", *addr, err)
	}

	if *selftest {
		runSelftest(ctx, rdb, *maxlen, *probe)
		return
	}
	if *reset {
		resetStream(ctx, rdb, *stream)
		return
	}
	if *watch {
		watchStream(ctx, rdb, *stream, *interval, *out)
		return
	}
	report(ctx, rdb, *stream)
}

// watchStream 周期性打印一行水位。
//
// 为什么不用 shell 脚本包一层：一轮采样要 spawn 好几个 awk/grep/cut 子进程，
// 在 Windows 上进程创建本身就要几十毫秒，实测一轮要好几秒——采样间隔会从
// 500ms 悄悄退化成十几秒，把曲线采样点打散。放在同一个进程里循环就没有这个开销。
func watchStream(ctx context.Context, rdb *redis.Client, stream string, interval time.Duration, out string) {
	var w *os.File
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			fatalf("创建 %s: %v", out, err)
		}
		defer f.Close()
		w = f
	} else {
		w = os.Stdout
	}

	line := func(format string, a ...any) {
		fmt.Fprintf(w, format, a...)
		// 每行都落盘，否则 tail -f 看不到实时曲线
		if w != os.Stdout {
			_ = w.Sync()
		}
	}

	line("%-8s %8s %6s %6s %8s %8s\n", "TIME", "XLEN", "DLQ", "ZSET", "LAG", "PENDING")
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		var xlen, dlq, zset int64 = -1, -1, -1
		if n, err := rdb.XLen(ctx, stream).Result(); err == nil {
			xlen = n
		}
		if n, err := rdb.XLen(ctx, stream+"_dlq").Result(); err == nil {
			dlq = n
		}
		if n, err := rdb.ZCard(ctx, stream+"_retry").Result(); err == nil {
			zset = n
		}
		var lag, pending int64 = -1, -1
		if groups, err := rdb.XInfoGroups(ctx, stream).Result(); err == nil && len(groups) > 0 {
			lag, pending = groups[0].Lag, groups[0].Pending
		}
		line("%-8s %8d %6d %6d %8d %8d\n",
			time.Now().Format("15:04:05"), xlen, dlq, zset, lag, pending)
	}
}

// resetStream 删除主 stream、DLQ 与延迟重投 ZSET。
//
// 注意：必须先把服务端停掉再执行。消费组仍存活时删 key 会让正在消费的
// XREADGROUP 拿到 NOGROUP 错误，反而污染观测结果。
func resetStream(ctx context.Context, rdb *redis.Client, stream string) {
	keys := []string{stream, stream + "_dlq", stream + "_retry"}
	n, err := rdb.Del(ctx, keys...).Result()
	if err != nil {
		fatalf("删除 %v: %v", keys, err)
	}
	fmt.Printf("已删除 %d 个 key: %v\n", n, keys)
}

// report 打印一个 stream 的容量水位。
func report(ctx context.Context, rdb *redis.Client, stream string) {
	fmt.Printf("stream        %s\n", stream)

	printLen(ctx, rdb, "XLEN", stream)
	printLen(ctx, rdb, "DLQ_XLEN", stream+"_dlq")

	if n, err := rdb.ZCard(ctx, stream+"_retry").Result(); err == nil {
		fmt.Printf("%-13s %d\n", "retry_zset", n)
	} else {
		fmt.Printf("%-13s (err: %v)\n", "retry_zset", err)
	}

	groups, err := rdb.XInfoGroups(ctx, stream).Result()
	if err != nil {
		fmt.Printf("%-13s (err: %v)\n", "groups", err)
	}
	for _, g := range groups {
		fmt.Printf("%-13s %s  consumers=%d  pending=%d  lag=%d\n",
			"group", g.Name, g.Consumers, g.Pending, g.Lag)
	}

	if info, err := rdb.Info(ctx, "memory").Result(); err == nil {
		for _, line := range strings.Split(info, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "used_memory_human:") {
				fmt.Printf("%-13s %s\n", "redis_mem", strings.TrimPrefix(line, "used_memory_human:"))
			}
		}
	}
}

func printLen(ctx context.Context, rdb *redis.Client, label, key string) {
	n, err := rdb.XLen(ctx, key).Result()
	if err != nil {
		fmt.Printf("%-13s (err: %v)\n", label, err)
		return
	}
	fmt.Printf("%-13s %d\n", label, n)
}

// runSelftest 回答两个问题：
//  1. 本机 Redis 是否接受 `XADD key MAXLEN ~ N`（近似裁剪）语法；
//  2. 裁剪是否真的生效——写 probe 条之后 XLEN 应该稳定在 maxlen 量级，而不是 probe。
//
// 这一步是必要的：miniredis 与真 Redis 在 `~` 的处理上并不完全一致，
// 而"支持语法"和"真的裁剪"是两件事，必须分别验证。
func runSelftest(ctx context.Context, rdb *redis.Client, maxlen, probe int64) {
	const key = "streamstat_selftest"
	_ = rdb.Del(ctx, key).Err()
	defer func() { _ = rdb.Del(ctx, key).Err() }()

	fmt.Printf("selftest key  %s\n", key)
	fmt.Printf("maxlen        %d\n", maxlen)
	fmt.Printf("probe         %d\n", probe)

	// 1. 精确裁剪（=）
	start := time.Now()
	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: key, MaxLen: maxlen, Approx: false,
		Values: map[string]any{"payload": "1"},
	}).Result(); err != nil {
		fmt.Printf("XADD MAXLEN = %d  -> 失败: %v\n", maxlen, err)
	} else {
		fmt.Printf("XADD MAXLEN = %d  -> 接受\n", maxlen)
	}
	_ = rdb.Del(ctx, key).Err()

	// 2. 近似裁剪（~），也就是生产代码要用的方式
	start = time.Now()
	var lastID string
	var failed error
	for i := int64(0); i < probe; i++ {
		id, err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: key, MaxLen: maxlen, Approx: true,
			Values: map[string]any{"payload": strconv.FormatInt(i, 10)},
		}).Result()
		if err != nil {
			failed = err
			break
		}
		lastID = id
	}
	elapsed := time.Since(start)
	if failed != nil {
		fmt.Printf("XADD MAXLEN ~ %d  -> 失败: %v\n", maxlen, failed)
		fmt.Println("结论：本机 Redis 不支持近似裁剪语法，生产代码需要改用 Approx=false 或升级 Redis")
		return
	}
	fmt.Printf("XADD MAXLEN ~ %d  -> 接受（最后 id=%s）\n", maxlen, lastID)

	n, err := rdb.XLen(ctx, key).Result()
	if err != nil {
		fmt.Printf("XLEN              -> 失败: %v\n", err)
		return
	}
	fmt.Printf("写入 %d 条后 XLEN   %d\n", probe, n)
	fmt.Printf("耗时              %s（%.0f msg/s）\n", elapsed.Round(time.Millisecond),
		float64(probe)/elapsed.Seconds())
	fmt.Printf("裁剪比            %.0f:1\n", float64(probe)/float64(n))
	if n <= maxlen*3 {
		fmt.Println("结论：近似裁剪生效，stream 长度有上界")
	} else {
		fmt.Println("结论：XLEN 未收敛到 MaxLen 量级，裁剪可能未生效")
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
