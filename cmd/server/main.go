package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"phantom-x/internal/proto"
	"phantom-x/internal/stream"
	"phantom-x/internal/transport"
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

	// 命令行参数覆盖
	listenAddr := flag.String("l", "", "监听地址")
	certFile := flag.String("cert", "", "TLS证书")
	keyFile := flag.String("key", "", "TLS私钥")
	token := flag.String("token", "", "认证令牌")
	wsPath := flag.String("path", "", "WebSocket路径")

	flag.Parse()

	if *showVersion {
		fmt.Printf("Phantom-X Server v%s\n", Version)
		fmt.Printf("  Build: %s\n", BuildTime)
		fmt.Printf("  Commit: %s\n", GitCommit)
		return
	}

	// 加载配置
	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		log.Fatalf("Load config failed: %v", err)
	}

	// 命令行参数覆盖
	if *listenAddr != "" {
		cfg.Listen = *listenAddr
	}
	if *certFile != "" {
		cfg.CertFile = *certFile
	}
	if *keyFile != "" {
		cfg.KeyFile = *keyFile
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *wsPath != "" {
		cfg.WSPath = *wsPath
	}

	// 创建服务
	server := NewServer(cfg)

	// 启动
	if err := server.Start(); err != nil {
		log.Fatalf("Start failed: %v", err)
	}

	printBanner(cfg)

	// 等待信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	server.Stop()
}

type Server struct {
	cfg       *config.ServerConfig
	upgrader  *transport.Upgrader
	streamMgr *stream.Manager
	httpSrv   *http.Server
}

func NewServer(cfg *config.ServerConfig) *Server {
	return &Server{
		cfg:       cfg,
		upgrader:  transport.NewUpgrader(cfg),
		streamMgr: stream.NewManager(),
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.WSPath, s.handleWebSocket)
	mux.HandleFunc("/", s.handleIndex)

	cert, err := tls.LoadX509KeyPair(s.cfg.CertFile, s.cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load cert failed: %w", err)
	}

	s.httpSrv = &http.Server{
		Addr:    s.cfg.Listen,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}

	go func() {
		if err := s.httpSrv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	return nil
}

func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.httpSrv.Shutdown(ctx)
	s.streamMgr.CloseAll()
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, clientID, err := s.upgrader.Upgrade(w, r)
	if err != nil {
		if err.Error() == "unauthorized" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}
		return
	}

	log.Printf("[Server] Client connected: %s", clientID)

	session := NewSession(clientID, conn, s.streamMgr, s.cfg)
	session.Serve()

	log.Printf("[Server] Client disconnected: %s", clientID)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<!DOCTYPE html><html><head><title>Welcome</title></head><body><h1>It works!</h1></body></html>`))
}

// Session 处理单个客户端连接
type Session struct {
	id        string
	conn      *transport.WSConn
	streamMgr *stream.Manager
	cfg       *config.ServerConfig
	writeCh   chan transport.WriteJob
	stopCh    chan struct{}
}

func NewSession(id string, conn *websocket.Conn, mgr *stream.Manager, cfg *config.ServerConfig) *Session {
	wsConn := transport.NewWSConn(0, conn, 4096)
	return &Session{
		id:        id,
		conn:      wsConn,
		streamMgr: mgr,
		cfg:       cfg,
		writeCh:   make(chan transport.WriteJob, 4096),
		stopCh:    make(chan struct{}),
	}
}

func (s *Session) Serve() {
	defer s.conn.Close()

	go s.writeLoop()
	s.readLoop()

	close(s.stopCh)
}

func (s *Session) writeLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case job := <-s.writeCh:
			s.conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			if err := s.conn.WriteMessage(websocket.BinaryMessage, job.Data); err != nil {
				return
			}
			metrics.IncrPacketsSent(1)
		case <-ticker.C:
			s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *Session) readLoop() {
	s.conn.SetPongHandler(func(string) error {
		s.conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		return nil
	})

	for {
		s.conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			if !transport.IsNormalClose(err) {
				log.Printf("[Server] Read error: %v", err)
			}
			return
		}

		if mt != websocket.BinaryMessage || len(data) < proto.HeaderLen {
			continue
		}

		metrics.IncrPacketsRecv(1)
		s.handleFrame(data)
	}
}

func (s *Session) handleFrame(data []byte) {
	cmd, streamID, flags, length := proto.UnpackHeader(data[:proto.HeaderLen])
	payload := data[proto.HeaderLen : proto.HeaderLen+length]

	// 处理 Padding
	if flags&proto.FlagPadding != 0 {
		payload = proto.RemovePadding(payload)
	}

	switch cmd {
	case proto.CmdOpenTCP:
		go s.handleTCPOpen(streamID, payload)
	case proto.CmdOpenUDP:
		go s.handleUDPOpen(streamID, payload)
	case proto.CmdData:
		s.handleData(streamID, payload)
	case proto.CmdClose:
		s.handleClose(streamID)
	}
}

func (s *Session) handleTCPOpen(streamID uint32, payload []byte) {
	ipStrategy, addr, port, initData, err := proto.ParseOpenPayload(payload)
	if err != nil {
		s.send(proto.CmdConnStatus, streamID, []byte{proto.StatusFail})
		return
	}

	target := fmt.Sprintf("%s:%d", addr, port)

	// 连接目标
	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("[Server] TCP dial failed: %s: %v", target, err)
		s.send(proto.CmdConnStatus, streamID, []byte{proto.StatusFail})
		return
	}

	st := stream.NewStream(streamID, target, false)
	st.TCPConn = conn
	st.SetState(stream.StateConnected)
	s.streamMgr.Register(st)

	// 0-RTT: 发送 InitData
	if len(initData) > 0 {
		conn.Write(initData)
		log.Printf("[Server] Sent InitData: %d bytes to %s", len(initData), target)
	}

	s.send(proto.CmdConnStatus, streamID, []byte{proto.StatusOK})
	log.Printf("[Server] TCP Stream: %d -> %s", streamID, target)

	go s.tcpReadLoop(st)
	_ = ipStrategy
}

func (s *Session) handleUDPOpen(streamID uint32, payload []byte) {
	ipStrategy, addr, port, _, err := proto.ParseOpenPayload(payload)
	if err != nil {
		s.send(proto.CmdConnStatus, streamID, []byte{proto.StatusFail})
		return
	}

	target := fmt.Sprintf("%s:%d", addr, port)

	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		s.send(proto.CmdConnStatus, streamID, []byte{proto.StatusFail})
		return
	}

	targetIP := net.ParseIP(addr)
	udpAddr := &net.UDPAddr{IP: targetIP, Port: int(port)}

	st := stream.NewStream(streamID, target, true)
	st.UDPConn = udpConn
	st.UDPAddr = udpAddr
	st.SetState(stream.StateConnected)
	s.streamMgr.Register(st)

	s.send(proto.CmdConnStatus, streamID, []byte{proto.StatusOK})
	log.Printf("[Server] UDP Stream: %d -> %s", streamID, target)

	go s.udpReadLoop(st)
	_ = ipStrategy
}

func (s *Session) handleData(streamID uint32, payload []byte) {
	st := s.streamMgr.Get(streamID)
	if st == nil {
		return
	}

	if _, err := st.Write(payload); err != nil {
		s.handleClose(streamID)
	}
}

func (s *Session) handleClose(streamID uint32) {
	s.streamMgr.Unregister(streamID)
}

func (s *Session) tcpReadLoop(st *stream.Stream) {
	bufPtr := transport.GetMediumBuf()
	buf := *bufPtr
	defer transport.PutMediumBuf(bufPtr)
	defer s.streamMgr.Unregister(st.ID)

	for {
		if st.IsClosed() {
			return
		}

		st.TCPConn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := st.TCPConn.Read(buf)
		if err != nil {
			s.send(proto.CmdClose, st.ID, nil)
			return
		}

		data := make([]byte, n)
		copy(data, buf[:n])
		s.send(proto.CmdData, st.ID, data)
	}
}

func (s *Session) udpReadLoop(st *stream.Stream) {
	bufPtr := transport.GetLargeBuf()
	buf := *bufPtr
	defer transport.PutLargeBuf(bufPtr)
	defer s.streamMgr.Unregister(st.ID)

	for {
		if st.IsClosed() {
			return
		}

		st.UDPConn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, addr, err := st.UDPConn.ReadFromUDP(buf)
		if err != nil {
			s.send(proto.CmdClose, st.ID, nil)
			return
		}

		// 构建响应
		host, portStr, _ := net.SplitHostPort(addr.String())
		var port int
		fmt.Sscanf(portStr, "%d", &port)

		payload := proto.BuildOpenPayload(0, host, uint16(port), buf[:n])
		s.send(proto.CmdData, st.ID, payload)
	}
}

func (s *Session) send(cmd byte, streamID uint32, payload []byte) {
	frame := proto.PackFrameAlloc(cmd, streamID, payload)
	select {
	case s.writeCh <- transport.WriteJob{Data: frame}:
	case <-time.After(100 * time.Millisecond):
		log.Printf("[Server] Write channel full for stream %d", streamID)
	}
}

func printBanner(cfg *config.ServerConfig) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║              Phantom-X Server v1.0                       ║")
	fmt.Println("║              高性能 · 抗探测 · 0-RTT                      ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║  监听: %-49s ║\n", cfg.Listen)
	fmt.Printf("║  路径: %-49s ║\n", cfg.WSPath)
	if cfg.Token != "" {
		fmt.Println("║  认证: 已启用                                            ║")
	} else {
		fmt.Println("║  认证: 未启用                                            ║")
	}
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Println("║  按 Ctrl+C 停止                                          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
}

// 需要导入 websocket 包
import "github.com/gorilla/websocket"
