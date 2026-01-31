package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"phantom-x/proto"
)

// ==================== 配置 ====================
var (
	serverAddr string
	localAddr  string
	token      string
	insecure   bool
	ipStrategy byte
	ipsFlag    string
)

func init() {
	flag.StringVar(&serverAddr, "s", "", "服务器地址 (wss://host:port/path)")
	flag.StringVar(&localAddr, "l", ":1080", "SOCKS5 监听地址")
	flag.StringVar(&token, "token", "", "认证令牌")
	flag.BoolVar(&insecure, "insecure", false, "跳过证书验证")
	flag.StringVar(&ipsFlag, "ips", "", "IP策略 (4/6/4,6/6,4)")
}

// ==================== 内存池 ====================
var bufPool = sync.Pool{New: func() any {
	b := make([]byte, proto.MaxPayload)
	return &b
}}

// ==================== 流状态 ====================
type StreamState struct {
	ID        uint32
	Target    string
	IsUDP     bool
	TCPConn   net.Conn
	UDPAssoc  *UDPAssociation
	DataCh    chan []byte
	Connected chan bool
	Closed    int32
}

func (s *StreamState) Close() {
	if !atomic.CompareAndSwapInt32(&s.Closed, 0, 1) {
		return
	}
	if s.TCPConn != nil {
		s.TCPConn.Close()
	}
	if s.UDPAssoc != nil {
		s.UDPAssoc.Close()
	}
	close(s.DataCh)
}

// ==================== 连接池 ====================
type ConnPool struct {
	serverURL string
	clientID  string
	
	mu       sync.RWMutex
	ws       *websocket.Conn
	writeCh  chan []byte
	streams  sync.Map // map[uint32]*StreamState
	
	streamCounter uint32
	closed        int32
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewConnPool(serverURL, clientID string) *ConnPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &ConnPool{
		serverURL: serverURL,
		clientID:  clientID,
		writeCh:   make(chan []byte, 4096),
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (p *ConnPool) Start() {
	go p.connectLoop()
}

func (p *ConnPool) connectLoop() {
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		
		ws, err := p.dial()
		if err != nil {
			log.Printf("[Client] Connect failed: %v, retrying...", err)
			time.Sleep(3 * time.Second)
			continue
		}
		
		p.mu.Lock()
		p.ws = ws
		p.mu.Unlock()
		
		log.Printf("[Client] Connected to server")
		
		// 启动读写协程
		go p.writeLoop(ws)
		p.readLoop(ws)
		
		p.mu.Lock()
		p.ws = nil
		p.mu.Unlock()
		
		// 清理所有流
		p.streams.Range(func(key, value any) bool {
			value.(*StreamState).Close()
			p.streams.Delete(key)
			return true
		})
		
		log.Printf("[Client] Disconnected, reconnecting...")
		time.Sleep(time.Second)
	}
}

func (p *ConnPool) dial() (*websocket.Conn, error) {
	u, err := url.Parse(p.serverURL)
	if err != nil {
		return nil, err
	}
	
	q := u.Query()
	q.Set("id", p.clientID)
	u.RawQuery = q.Encode()
	
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecure,
			MinVersion:         tls.VersionTLS12,
		},
		HandshakeTimeout: 10 * time.Second,
	}
	
	if token != "" {
		dialer.Subprotocols = []string{token}
	}
	
	conn, resp, err := dialer.Dial(u.String(), nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, errors.New("authentication failed")
		}
		return nil, err
	}
	
	return conn, nil
}

func (p *ConnPool) writeLoop(ws *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-p.ctx.Done():
			return
		case data, ok := <-p.writeCh:
			if !ok {
				return
			}
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
				ws.Close()
				return
			}
		case <-ticker.C:
			ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				ws.Close()
				return
			}
		}
	}
}

func (p *ConnPool) readLoop(ws *websocket.Conn) {
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	
	for {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		
		if mt != websocket.BinaryMessage || len(data) < proto.HeaderLen {
			continue
		}
		
		cmd, streamID, length := proto.UnpackHeader(data[:proto.HeaderLen])
		if len(data) < proto.HeaderLen+length {
			continue
		}
		payload := data[proto.HeaderLen : proto.HeaderLen+length]
		
		switch cmd {
		case proto.CmdConnStatus:
			if v, ok := p.streams.Load(streamID); ok {
				st := v.(*StreamState)
				if len(payload) > 0 && payload[0] == proto.StatusOK {
					select {
					case st.Connected <- true:
					default:
					}
				} else {
					st.Close()
					p.streams.Delete(streamID)
				}
			}
			
		case proto.CmdData:
			if v, ok := p.streams.Load(streamID); ok {
				st := v.(*StreamState)
				dataCopy := make([]byte, len(payload))
				copy(dataCopy, payload)
				select {
				case st.DataCh <- dataCopy:
				case <-time.After(5 * time.Second):
				}
			}
			
		case proto.CmdClose:
			if v, ok := p.streams.Load(streamID); ok {
				v.(*StreamState).Close()
				p.streams.Delete(streamID)
			}
		}
	}
}

func (p *ConnPool) Send(data []byte) error {
	select {
	case p.writeCh <- data:
		return nil
	case <-time.After(100 * time.Millisecond):
		return errors.New("write channel full")
	}
}

func (p *ConnPool) NewStream() uint32 {
	return atomic.AddUint32(&p.streamCounter, 1)
}

func (p *ConnPool) RegisterStream(st *StreamState) {
	p.streams.Store(st.ID, st)
}

func (p *ConnPool) UnregisterStream(id uint32) {
	if v, ok := p.streams.LoadAndDelete(id); ok {
		v.(*StreamState).Close()
	}
}

// ==================== UDP 关联 ====================
type UDPAssociation struct {
	connID      uint32
	pool        *ConnPool
	udpListener *net.UDPConn
	clientAddr  *net.UDPAddr
	tcpConn     net.Conn
	closed      int32
	mu          sync.Mutex
}

func (a *UDPAssociation) Close() {
	if !atomic.CompareAndSwapInt32(&a.closed, 0, 1) {
		return
	}
	if a.udpListener != nil {
		a.udpListener.Close()
	}
}

// ==================== SOCKS5 处理 ====================
func handleSocks5(conn net.Conn, pool *ConnPool) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	
	// 握手
	buf := make([]byte, 256)
	if _, err := io.ReadFull(conn, buf[:2]); err != nil || buf[0] != 0x05 {
		return
	}
	nmethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nmethods]); err != nil {
		return
	}
	conn.Write([]byte{0x05, 0x00})
	
	// 请求
	if _, err := io.ReadFull(conn, buf[:4]); err != nil || buf[0] != 0x05 {
		return
	}
	
	cmd := buf[1]
	atyp := buf[3]
	
	var host string
	switch atyp {
	case 0x01: // IPv4
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			return
		}
		host = net.IP(buf[:4]).String()
	case 0x03: // Domain
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		domainLen := int(buf[0])
		if _, err := io.ReadFull(conn, buf[:domainLen]); err != nil {
			return
		}
		host = string(buf[:domainLen])
	case 0x04: // IPv6
		if _, err := io.ReadFull(conn, buf[:16]); err != nil {
			return
		}
		host = net.IP(buf[:16]).String()
	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(buf[:2])
	
	conn.SetDeadline(time.Time{})
	
	switch cmd {
	case 0x01: // CONNECT
		handleSocks5Connect(conn, pool, host, port)
	case 0x03: // UDP ASSOCIATE
		handleSocks5UDP(conn, pool)
	default:
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}
}

func handleSocks5Connect(conn net.Conn, pool *ConnPool, host string, port uint16) {
	// 回复成功 (乐观模式)
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	
	// ========== 0-RTT: 预读首包数据 ==========
	conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	initBuf := make([]byte, 4096)
	n, _ := conn.Read(initBuf)
	conn.SetReadDeadline(time.Time{})
	initData := initBuf[:n]
	
	target := fmt.Sprintf("%s:%d", host, port)
	streamID := pool.NewStream()
	
	st := &StreamState{
		ID:        streamID,
		Target:    target,
		TCPConn:   conn,
		DataCh:    make(chan []byte, 64),
		Connected: make(chan bool, 1),
	}
	pool.RegisterStream(st)
	
	// 构建 Open 请求
	payload := proto.BuildOpenPayload(ipStrategy, host, port, initData)
	pool.Send(proto.PackFrameAlloc(proto.CmdOpenTCP, streamID, payload))
	
	log.Printf("[Client] TCP: %s (InitData: %d bytes)", target, len(initData))
	
	defer func() {
		pool.Send(proto.PackFrameAlloc(proto.CmdClose, streamID, nil))
		pool.UnregisterStream(streamID)
	}()
	
	// Uplink: 本地 -> 远程
	go func() {
		bufPtr := bufPool.Get().(*[]byte)
		buf := *bufPtr
		defer bufPool.Put(bufPtr)
		
		for {
			nr, err := conn.Read(buf)
			if nr > 0 {
				data := make([]byte, nr)
				copy(data, buf[:nr])
				pool.Send(proto.PackFrameAlloc(proto.CmdData, streamID, data))
			}
			if err != nil {
				return
			}
		}
	}()
	
	// Downlink: 远程 -> 本地
	for data := range st.DataCh {
		if _, err := conn.Write(data); err != nil {
			return
		}
	}
}

func handleSocks5UDP(conn net.Conn, pool *ConnPool) {
	// 创建本地 UDP 监听
	localIP := conn.LocalAddr().(*net.TCPAddr).IP
	udpAddr := &net.UDPAddr{IP: localIP, Port: 0}
	udpListener, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	
	actualAddr := udpListener.LocalAddr().(*net.UDPAddr)
	
	// 回复 UDP 绑定地址
	resp := []byte{0x05, 0x00, 0x00}
	if ip4 := actualAddr.IP.To4(); ip4 != nil {
		resp = append(resp, 0x01)
		resp = append(resp, ip4...)
	} else {
		resp = append(resp, 0x04)
		resp = append(resp, actualAddr.IP...)
	}
	resp = append(resp, byte(actualAddr.Port>>8), byte(actualAddr.Port))
	conn.Write(resp)
	
	streamID := pool.NewStream()
	
	assoc := &UDPAssociation{
		connID:      streamID,
		pool:        pool,
		udpListener: udpListener,
		tcpConn:     conn,
	}
	
	st := &StreamState{
		ID:        streamID,
		IsUDP:     true,
		UDPAssoc:  assoc,
		DataCh:    make(chan []byte, 64),
		Connected: make(chan bool, 1),
	}
	pool.RegisterStream(st)
	
	// UDP 读取循环
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := udpListener.ReadFromUDP(buf)
			if err != nil {
				return
			}
			
			assoc.mu.Lock()
			if assoc.clientAddr == nil {
				assoc.clientAddr = addr
			}
			assoc.mu.Unlock()
			
			// 解析 SOCKS5 UDP 包
			target, data, err := parseSocks5UDP(buf[:n])
			if err != nil {
				continue
			}
			
			host, portStr, _ := net.SplitHostPort(target)
			var port int
			fmt.Sscanf(portStr, "%d", &port)
			
			// 发送 OpenUDP (首次) 或 Data
			payload := proto.BuildOpenPayload(ipStrategy, host, uint16(port), data)
			pool.Send(proto.PackFrameAlloc(proto.CmdData, streamID, payload))
		}
	}()
	
	// 接收远程数据并转发
	go func() {
		for data := range st.DataCh {
			// 解析返回的数据
			_, addr, port, payload, err := proto.ParseOpenPayload(data)
			if err != nil || len(payload) == 0 {
				continue
			}
			
			// 构建 SOCKS5 UDP 响应
			resp := buildSocks5UDP(addr, int(port), payload)
			
			assoc.mu.Lock()
			clientAddr := assoc.clientAddr
			assoc.mu.Unlock()
			
			if clientAddr != nil {
				udpListener.WriteToUDP(resp, clientAddr)
			}
		}
	}()
	
	// 等待 TCP 连接断开
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}
	
	pool.Send(proto.PackFrameAlloc(proto.CmdClose, streamID, nil))
	pool.UnregisterStream(streamID)
}

func parseSocks5UDP(b []byte) (string, []byte, error) {
	if len(b) < 10 || b[2] != 0 {
		return "", nil, errors.New("invalid")
	}
	off := 4
	var host string
	switch b[3] {
	case 0x01:
		host = net.IP(b[off : off+4]).String()
		off += 4
	case 0x03:
		l := int(b[off])
		off++
		host = string(b[off : off+l])
		off += l
	case 0x04:
		host = net.IP(b[off : off+16]).String()
		off += 16
	default:
		return "", nil, errors.New("invalid atyp")
	}
	port := int(b[off])<<8 | int(b[off+1])
	off += 2
	return fmt.Sprintf("%s:%d", host, port), b[off:], nil
}

func buildSocks5UDP(host string, port int, data []byte) []byte {
	buf := []byte{0, 0, 0}
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		buf = append(buf, 0x01)
		buf = append(buf, ip4...)
	} else if ip != nil {
		buf = append(buf, 0x04)
		buf = append(buf, ip...)
	} else {
		buf = append(buf, 0x03, byte(len(host)))
		buf = append(buf, host...)
	}
	buf = append(buf, byte(port>>8), byte(port))
	buf = append(buf, data...)
	return buf
}

func parseIPStrategy(s string) byte {
	s = strings.TrimSpace(s)
	switch s {
	case "4":
		return proto.IPv4Only
	case "6":
		return proto.IPv6Only
	case "4,6":
		return proto.IPv4First
	case "6,4":
		return proto.IPv6First
	default:
		return proto.IPDefault
	}
}

// ==================== 主函数 ====================
func main() {
	flag.Parse()
	
	if serverAddr == "" {
		log.Fatal("Usage: phantom-x-client -s wss://host:port/path -l :1080")
	}
	
	ipStrategy = parseIPStrategy(ipsFlag)
	clientID := uuid.NewString()
	
	log.Printf("[Client] Phantom-X Client starting...")
	log.Printf("[Client] Server: %s", serverAddr)
	log.Printf("[Client] SOCKS5: %s", localAddr)
	log.Printf("[Client] ID: %s", clientID[:8])
	
	pool := NewConnPool(serverAddr, clientID)
	pool.Start()
	
	// 等待首次连接成功
	time.Sleep(time.Second)
	
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		log.Fatalf("[Client] Listen failed: %v", err)
	}
	
	log.Printf("[Client] Ready!")
	
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleSocks5(conn, pool)
	}
}
