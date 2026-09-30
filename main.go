// vvae-notifier：VVAE 推送的自部署轮询程序。
// 读 VVAE_KEY 里的一个或多个部署串，每个各跑一个 Poller，状态存 DATA_DIR/state-<通道标识>.json。
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/vastlogic-dev/vvae-notifier/internal/notifier"
)

// version 由构建参数 -ldflags "-X main.version=<版本>" 写入。
var version = "dev"

// logger 的 prefix 是 "[通道标识] "，多个账号时分得清是哪一个；进程级日志为空。
type logger struct{ prefix string }

func stamp() string { return time.Now().UTC().Format("2006-01-02 15:04:05Z") }

func (l logger) Infof(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "%s %s%s\n", stamp(), l.prefix, fmt.Sprintf(format, args...))
}

func (l logger) Warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s [warn] %s%s\n", stamp(), l.prefix, fmt.Sprintf(format, args...))
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func dataDir() string {
	if d := os.Getenv("DATA_DIR"); d != "" {
		return d
	}
	if st, err := os.Stat("/data"); err == nil && st.IsDir() {
		return "/data"
	}
	return "data"
}

func main() {
	if version == "" {
		version = "dev" // 中继拒收空版本号的心跳
	}
	raw := os.Getenv("VVAE_KEY")
	if raw == "" {
		fail("缺少环境变量 VVAE_KEY：在 VVAE App 的推送设置里复制部署串，用 -e VVAE_KEY=... 传入。")
	}
	deploys, err := notifier.ParseDeployStrings(raw)
	if err != nil {
		fail(err.Error())
	}
	log := logger{}
	dir := dataDir()
	if err := notifier.NewFileStore(dir, "").Writable(); err != nil {
		log.Warnf("状态目录 %s 无法写入（%v）。程序照常运行，但重启后会重新记基线、可能漏掉重启期间的提醒。"+
			"挂载宿主机目录时，把目录属主改成 65532（chown 65532:65532 <目录>），或用 --user 指定能写入的用户。", dir, err)
	}
	client := &http.Client{Timeout: 30 * time.Second} // 走 HTTPS_PROXY 等代理环境变量
	// 容器里默认是容器 ID；App 给的命令带 --hostname "$(hostname)"，这里就是宿主机的名字
	hostname, _ := os.Hostname()
	log.Infof("vvae-notifier %s 启动，%d 个部署串，状态目录 %s", version, len(deploys), dir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// 先全部建好再启动：有一个建不了就退出，不让前面的已经发出心跳
	type job struct {
		p   *notifier.Poller
		log logger
	}
	var jobs []job
	for _, d := range deploys {
		tag := d.Tag()
		plog := logger{prefix: "[" + tag + "] "}
		p, err := notifier.New(notifier.Options{
			Deploy:   d,
			Store:    notifier.NewFileStore(dir, "state-"+tag+".json"),
			Client:   client,
			Version:  version,
			Log:      plog,
			Hostname: hostname,
			APIShare: len(deploys),
		})
		if err != nil {
			fail(err.Error())
		}
		relayHost := d.Relay
		if u, err := url.Parse(d.Relay); err == nil {
			relayHost = u.Host
		}
		plog.Infof("中继 %s，实例 %s，主机名 %s", relayHost, p.InstanceTag(), hostname)
		jobs = append(jobs, job{p, plog})
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(ctx, j.p, j.log)
		}()
	}
	wg.Wait()
	log.Infof("已退出")
}

// run 循环执行一个部署串的 Poller，直到收到退出信号。
func run(ctx context.Context, p *notifier.Poller, log logger) {
	for {
		timer := time.NewTimer(tick(p, log))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// tick 跑一轮；意外 panic 时记日志、一分钟后再试，不让进程退出。
func tick(p *notifier.Poller, log logger) (delay time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Warnf("本轮出错：%v\n%s", r, debug.Stack())
			delay = time.Minute
		}
	}()
	return p.Tick()
}
