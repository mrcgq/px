package socks5

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"phantom-x/internal/proto"
	"phantom-x/internal/stream"
	"phantom-x/internal/transport"
	"phantom-x/pkg/config"
	"phantom-x/pkg/metrics"
)

// ==================== SOCKS5 常量 ====================

const (
	Version5       = 0x05
	AuthNone       = 0x00
	AuthUserPass   = 0x02
	CmdConnect     = 0x01
	CmdBind        = 0x02
	CmdUDPAssoc    = 0x03
	AtypIPv4       = 0x01
	AtypDomain     = 0x03
	AtypIPv6       = 0x04
	RepSuccess     = 0x00
	RepServerFail  = 0x01
	RepNotAllowed  = 0x02
	RepNetUnreach  = 0x03
	RepHostUnreach = 0x04
	RepConnRefused = 0x05
	RepTTLExpired  = 0x06
	RepCmdNotSupp  = 0x07
	RepAtypNotSupp = 0x08
)

// ==================== 发送回调 ====================

type SendFunc func(cmd byte, streamID uint32, payload []byte) error
type BroadcastFunc func(cmd byte, streamID uint32, payload []byte) error

// ==================== SOCKS5 服务器 ====================

type Server struct {
	cfg       *config.ClientConfig
	listener  net.Listener
	streamMgr *stream.Manager

	username string
	password string

	sendFunc      SendFunc
	broadcastFunc BroadcastFunc
	getUplinkFunc func(uint32) (int, bool)

	ipStrategy byte
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

func NewServer(cfg *config.ClientConfig, mgr *stream.Manager) *Server {
	s := &Server{
		cfg:        cfg,
		streamMgr:  mgr,
		ipStrategy: parseIPStrategy(cfg.IPStrategy),
		stopCh:     make(chan struct{}),
	}

	// 解析认证信息
	if cfg.Socks5Auth != "" {
		parts := strings.SplitN(cfg.Socks5Auth, ":", 2)
		if len(parts) == 2 {
			s.username = parts[0]
			s.password = parts[1]
		}
	}

	return s
}

func (s *Server) SetSendFunc(f SendFunc) {
	s.sendFunc = f
}

func (s *Server) SetBroadcastFunc(f BroadcastFunc) {
	s.broadcastFunc = f
}

func (s *Server) SetGetUplinkFunc(f func(uint32) (int, bool)) {
	s.getUplinkFunc = f
}

func (s *Server) Start() error {
	addr := s.cfg.Socks5Listen
	if addr == "" {
		addr = ":1080"
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	s.listener = listener

	s.wg.Add(1)
	go s.acceptLoop()

	log.Printf("[SOCKS5] Listening on %s", addr)
	return nil
}

func (s *Server) Stop() {
	close(s.stopCh)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				continue
			}
		}

		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// 1. 握手
	if err := s.handshake(conn); err != nil {
		return
	}

	// 2. 读取请求
	cmd, atyp, addr, port, err := s.readRequest(conn)
	if err != nil {
		return
	}

	conn.SetDeadline(time.Time{})

	target := fmt.Sprintf("%s:%d", addr, port)

	switch cmd {
	case CmdConnect:
		s.handleConnect(conn, target, atyp)
	case CmdUDPAssoc:
		s.handleUDPAssociate(conn)
	default:
		s.sendReply(conn, RepCmdNotSupp, nil)
	}
}

func (s *Server) handshake(conn net.Conn) error {
	buf := make([]byte, 256)

	// 读取版本和方法数
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return err
	}
	if buf[0] != Version5 {
		return errors.New("invalid version")
	}

	nmethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nmethods]); err != nil {
		return err
	}

	// 选择认证方法
	if s.username != "" {
		conn.Write([]byte{Version5, AuthUserPass})
		return s.authenticateUserPass(conn)
	}

	conn.Write([]byte{Version5, AuthNone})
	return nil
}

func (s *Server) authenticateUserPass(conn net.Conn) error {
	buf := make([]byte, 256)

	// 版本
	if _, err := io.ReadFull(conn, buf[:1]); err != nil {
		return err
	}
	if buf[0] != 0x01 {
		return errors.New("invalid auth version")
	}

	// 用户名
	if _, err := io.ReadFull(conn, buf[:1]); err != nil {
		return err
	}
	ulen := int(buf[0])
	if _, err := io.ReadFull(conn, buf[:ulen]); err != nil {
		return err
	}
	username := string(buf[:ulen])

	// 密码
	if _, err := io.ReadFull(conn, buf[:1]); err != nil {
		return err
	}
	plen := int(buf[0])
	if _, err := io.ReadFull(conn, buf[:plen]); err != nil {
		return err
	}
	password := string(buf[:plen])

	// 验证
	if username == s.username && password == s.password {
		conn.Write([]byte{0x01, 0x00})
		return nil
	}

	conn.Write([]byte{0x01, 0x01})
	return errors.New("auth failed")
}

func (s *Server) readRequest(conn net.Conn) (cmd, atyp byte, addr string, port uint16, err error) {
	buf := make([]byte, 256)

	if _, err = io.ReadFull(conn, buf[:4]); err != nil {
		return
	}
	if buf[0] != Version5 {
		err = errors.New("invalid version")
		return
	}

	cmd = buf[1]
	atyp = buf[3]

	switch atyp {
	case AtypIPv4:
		if _, err = io.ReadFull(conn, buf[:4]); err != nil {
			return
		}
		addr = net.IP(buf[:4]).String()

	case AtypDomain:
		if _, err = io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		domainLen := int(buf[0])
		if _, err = io.ReadFull(conn, buf[:domainLen]); err != nil {
			return
		}
		addr = string(buf[:domainLen])

	case AtypIPv6:
		if _, err = io.ReadFull(conn, buf[:16]); err != nil {
			return
		}
		addr = net.IP(buf[:16]).String()

	default:
		err = errors.New("unsupported address type")
		return
	}

	if _, err = io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	port = binary.BigEndian.Uint16(buf[:2])

	return
}

func (s *Server) sendReply(conn net.Conn, rep byte, bindAddr net.Addr) {
	reply := []byte{Version5, rep, 0x00, AtypIPv4, 0, 0, 0, 0, 0, 0}

	if bindAddr != nil {
		if tcpAddr, ok := bindAddr.(*net.TCPAddr); ok {
			if ip4 := tcpAddr.IP.To4(); ip4 != nil {
				reply[3] = AtypIPv4
				copy(reply[4:8], ip4)
				reply[8] = byte(tcpAddr.Port >> 8)
				reply[9] = byte(tcpAddr.Port)
			}
		}
	}

	conn.Write(reply)
}

func (s *Server) handleConnect(conn net.Conn, target string, atyp byte) {
	// IP 策略检查
	if !s.checkIPStrategy(target, atyp) {
		s.sendReply(conn, RepNotAllowed, nil)
		return
	}

	// 乐观模式：立即回复成功
	s.sendReply(conn, RepSuccess, nil)

	// 0-RTT: 预读首包数据
	conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	initBuf := make([]byte, proto.MaxInitData)
	n, _ := conn.Read(initBuf)
	conn.SetReadDeadline(time.Time{})
	initData := initBuf[:n]

	// 解析目标地址
	host, portStr, _ := net.SplitHostPort(target)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// 创建流
	streamID := s.streamMgr.NewStreamID()
	st := stream.NewStream(streamID, target, false)
	st.SetState(stream.StateConnecting)
	s.streamMgr.Register(st)

	// 构建 Open 请求
	payload := proto.BuildOpenPayload(s.ipStrategy, host, uint16(port), initData)

	// 广播发送
	if s.broadcastFunc != nil {
		s.broadcastFunc(proto.CmdOpenTCP, streamID, payload)
	}

	log.Printf("[SOCKS5] CONNECT %s (InitData: %d bytes)", target, len(initData))
	metrics.IncrActiveStreams()

	// 启动双向转发
	s.relay(conn, st)
}

func (s *Server) relay(conn net.Conn, st *stream.Stream) {
	defer func() {
		// 发送关闭
		if s.sendFunc != nil {
			if connID, ok := s.getUplinkFunc(st.ID); ok {
				s.sendFunc(proto.CmdClose, st.ID, nil)
				_ = connID
			} else if s.broadcastFunc != nil {
				s.broadcastFunc(proto.CmdClose, st.ID, nil)
			}
		}
		conn.Close()
		s.streamMgr.Unregister(st.ID)
	}()

	// Uplink: 本地 -> 远程
	go func() {
		bufPtr := transport.GetMediumBuf()
		buf := *bufPtr
		defer transport.PutMediumBuf(bufPtr)

		for {
			select {
			case <-st.CloseCh:
				return
			default:
			}

			nr, err := conn.Read(buf)
			if err != nil {
				return
			}

			data := make([]byte, nr)
			copy(data, buf[:nr])

			if connID, ok := s.getUplinkFunc(st.ID); ok {
				if s.sendFunc != nil {
					s.sendFunc(proto.CmdData, st.ID, data)
				}
				_ = connID
			} else if s.broadcastFunc != nil {
				s.broadcastFunc(proto.CmdData, st.ID, data)
			}

			metrics.AddBytesSent(int64(nr))
		}
	}()

	// Downlink: 远程 -> 本地
	for {
		select {
		case data, ok := <-st.DataCh:
			if !ok {
				return
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
			metrics.AddBytesRecv(int64(len(data)))

		case <-st.CloseCh:
			return
		}
	}
}

func (s *Server) handleUDPAssociate(conn net.Conn) {
	// 创建本地 UDP 监听
	localIP := conn.LocalAddr().(*net.TCPAddr).IP
	udpAddr := &net.UDPAddr{IP: localIP, Port: 0}
	udpListener, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		s.sendReply(conn, RepServerFail, nil)
		return
	}

	actualAddr := udpListener.LocalAddr().(*net.UDPAddr)
	s.sendReply(conn, RepSuccess, &net.TCPAddr{IP: actualAddr.IP, Port: actualAddr.Port})

	streamID := s.streamMgr.NewStreamID()
	st := stream.NewStream(streamID, "udp-assoc", true)
	st.UDPConn = udpListener
	s.streamMgr.Register(st)

	log.Printf("[SOCKS5] UDP ASSOCIATE on %s", actualAddr)

	// UDP 处理
	go s.handleUDPRelay(st, udpListener)

	// 等待 TCP 连接关闭
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}

	st.Close()
	s.streamMgr.Unregister(streamID)
}

func (s *Server) handleUDPRelay(st *stream.Stream, udpListener *net.UDPConn) {
	bufPtr := transport.GetLargeBuf()
	buf := *bufPtr
	defer transport.PutLargeBuf(bufPtr)

	for {
		select {
		case <-st.CloseCh:
			return
		default:
		}

		udpListener.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, clientAddr, err := udpListener.ReadFromUDP(buf)
		if err != nil {
			return
		}

		if st.UDPClientAddr == nil {
			st.UDPClientAddr = clientAddr
		}

		// 解析 SOCKS5 UDP 包
		target, data, err := ParseUDPPacket(buf[:n])
		if err != nil {
			continue
		}

		// 发送到远程
		host, portStr, _ := net.SplitHostPort(target)
		var port int
		fmt.Sscanf(portStr, "%d", &port)

		payload := proto.BuildOpenPayload(s.ipStrategy, host, uint16(port), data)

		if s.broadcastFunc != nil {
			s.broadcastFunc(proto.CmdData, st.ID, payload)
		}
	}
}

func (s *Server) checkIPStrategy(target string, atyp byte) bool {
	host, _, _ := net.SplitHostPort(target)
	ip := net.ParseIP(host)

	switch s.ipStrategy {
	case proto.IPv4Only:
		if atyp == AtypIPv6 || (ip != nil && ip.To4() == nil) {
			return false
		}
	case proto.IPv6Only:
		if atyp == AtypIPv4 || (ip != nil && ip.To4() != nil) {
			return false
		}
	}
	return true
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

// ==================== UDP 包解析 ====================

func ParseUDPPacket(b []byte) (target string, data []byte, err error) {
	if len(b) < 10 || b[2] != 0 {
		return "", nil, errors.New("invalid packet")
	}

	off := 4
	var host string

	switch b[3] {
	case AtypIPv4:
		if off+4 > len(b) {
			return "", nil, errors.New("too short")
		}
		host = net.IP(b[off : off+4]).String()
		off += 4

	case AtypDomain:
		if off+1 > len(b) {
			return "", nil, errors.New("too short")
		}
		l := int(b[off])
		off++
		if off+l > len(b) {
			return "", nil, errors.New("too short")
		}
		host = string(b[off : off+l])
		off += l

	case AtypIPv6:
		if off+16 > len(b) {
			return "", nil, errors.New("too short")
		}
		host = net.IP(b[off : off+16]).String()
		off += 16

	default:
		return "", nil, errors.New("invalid atyp")
	}

	if off+2 > len(b) {
		return "", nil, errors.New("too short")
	}

	port := int(b[off])<<8 | int(b[off+1])
	off += 2

	target = fmt.Sprintf("%s:%d", host, port)
	data = b[off:]

	return
}

func BuildUDPPacket(host string, port int, data []byte) []byte {
	buf := []byte{0, 0, 0}

	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		buf = append(buf, AtypIPv4)
		buf = append(buf, ip4...)
	} else if ip != nil {
		buf = append(buf, AtypIPv6)
		buf = append(buf, ip...)
	} else {
		buf = append(buf, AtypDomain, byte(len(host)))
		buf = append(buf, host...)
	}

	buf = append(buf, byte(port>>8), byte(port))
	buf = append(buf, data...)

	return buf
}
