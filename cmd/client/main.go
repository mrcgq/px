package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"phantom-x/internal/ech"
	"phantom-x/internal/pool"
	"phantom-x/internal/socks5"
	"phantom-x/internal/stream"
	"phantom-x/pkg/config"
	"phantom-x/pkg/metrics"
)

var (
	Version   = "1.0.0"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

func main() {
	configPath := flag.String("c", "", "配置文件路径")
	showVersion := flag.Bool("v", false, "显示版本")
	showStats := flag.Bool("stats", false, "退出时显示统计")

	// 命令行参数覆盖
	serverAddr := flag.String("s", "", "服务器地址")
	token := flag.String("token", "", "认证令牌")
	socksAddr := flag.String("l", "", "SOCKS5监听地址")
	insecure := flag.Bool("insecure", false, "跳过证书验证")
	noECH := flag.Bool("no-ech", false, "禁用ECH")

	flag.Parse()

	if *showVersion {
		fmt.Printf("Phantom-X Client v%s\n", Version)
		fmt.Printf("  Build: %s\n", BuildTime)
		fmt.Printf("  Commit: %s\n", GitCommit)
		return
	}

	// 加载配置
	cfg, err := config.LoadClientConfig(*configPath)
	if err != nil {
		log.Printf("Load config failed, using defaults: %v", err)
		cfg = config.DefaultClientConfig()
	}

	// 命令行参数覆盖
	if *serverAddr != "" {
		cfg.Server = *serverAddr
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *socksAddr != "" {
		cfg.Socks5Listen = *socksAddr
	}
	if *insecure {
		cfg.Insecure = true
	}
	if *noECH {
		cfg.EnableECH = false
	}

	// 验证配置
	if cfg.Server == "" {
		log.Fatal("Server address is required")
	}

	// 生成客户端 ID
	if cfg.ClientID == "" {
		cfg.ClientID = uuid.NewString()
	}

	// 初始化 ECH
	if cfg.EnableECH && !cfg.Insecure {
		log.Println("Preparing ECH...")
		if err := ech.Prepare(cfg.ECHDomain, cfg.ECHDns); err != nil {
			log.Printf("ECH prepare failed, falling back to TLS: %v", err)
			cfg.EnableECH = false
		}
	}

	// 创建流管理器
	streamMgr := stream.NewManager()

	// 创建连接池
	poolCfg := &pool.Config{
		ServerURL:      cfg.Server,
		Token:          cfg.Token,
		ClientID:       cfg.ClientID,
		Insecure:       cfg.Insecure,
		NumConnections: cfg.NumConnections,
		WriteQueueSize: 4096,
		WriteTimeout:   cfg.WriteTimeout,
		ReadTimeout:    cfg.ReadTimeout,
		PingInterval:   30 * time.Second,
		ReconnectDelay: time.Second,
		MaxBackoff:     30 * time.Second,
		EnableECH:      cfg.EnableECH,
		ECHDomain:      cfg.ECHDomain,
		ECHDns:         cfg.ECHDns,
		EnablePadding:  cfg.EnablePadding,
		PaddingMinSize: cfg.PaddingMinSize,
		PaddingMaxSize: cfg.PaddingMaxSize,
	}

	connPool := pool.NewConnPool(poolCfg)

	// 设置帧处理回调
	connPool.SetFrameHandler(func(streamID uint32, cmd byte, payload []byte) {
		handleServerFrame(streamMgr, streamID, cmd, payload)
	})

	// 启动连接池
	if err := connPool.Start(); err != nil {
		log.Fatalf("Start connection pool failed: %v", err)
	}

	// 等待连接建立
	time.Sleep(time.Second)

	// 创建 SOCKS5 服务器
	socks5Server := socks5.NewServer(cfg, streamMgr)
	socks5Server.SetSendFunc(func(cmd byte, streamID uint32, payload []byte) error {
		return connPool.SendTo(0, proto.PackFrameAlloc(cmd, streamID, payload))
	})
	socks5Server.SetBroadcastFunc(func(cmd byte, streamID uint32, payload []byte) error {
		return connPool.Broadcast(proto.PackFrameAlloc(cmd, streamID, payload))
	})
	socks5Server.SetGetUplinkFunc(func(id uint32) (int, bool) {
		st := streamMgr.Get(id)
		if st != nil && st.ConnID >= 0 {
			return st.ConnID, true
		}
		return 0, false
	})

	if err := socks5Server.Start(); err != nil {
		log.Fatalf("Start SOCKS5 failed: %v", err)
	}

	printBanner(cfg)

	// 等待信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	socks5Server.Stop()
	connPool.Stop()
	streamMgr.CloseAll()

	if *showStats {
		printStats()
	}
}

func handleServerFrame(mgr *stream.Manager, streamID uint32, cmd byte, payload []byte) {
	switch cmd {
	case proto.CmdConnStatus:
		st := mgr.Get(streamID)
		if st == nil {
			return
		}
		if len(payload) > 0 && payload[0] == proto.StatusOK {
			st.SetState(stream.StateConnected)
			st.SignalConnected()
		} else {
			mgr.Unregister(streamID)
		}

	case proto.CmdData:
		st := mgr.Get(streamID)
		if st != nil {
			st.SendData(payload)
		}

	case proto.CmdClose:
		mgr.Unregister(streamID)
	}
}

func printBanner(cfg *config.ClientConfig) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║              Phantom-X Client v1.0                       ║")
	fmt.Println("║              高性能 · 抗探测 · 0-RTT                      ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║  服务器: %-47s ║\n", cfg.Server)
	fmt.Printf("║  SOCKS5: %-47s ║\n", cfg.Socks5Listen)
	fmt.Printf("║  连接数: %-47d ║\n", cfg.NumConnections)
	echStatus := "已禁用"
	if cfg.EnableECH {
		echStatus = "已启用"
	}
	fmt.Printf("║  ECH:    %-47s ║\n", echStatus)
	paddingStatus := "已禁用"
	if cfg.EnablePadding {
		paddingStatus = "已启用"
	}
	fmt.Printf("║  Padding: %-46s ║\n", paddingStatus)
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Println("║  按 Ctrl+C 停止  |  --stats 查看统计                     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func printStats() {
	stats := metrics.GetStats()
	fmt.Println()
	fmt.Println("══════════════════ 统计信息 ══════════════════")
	fmt.Println(stats.String())
	fmt.Println("═══════════════════════════════════════════════")
}

// 需要导入 proto 包
import "phantom-x/internal/proto"
