package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"phantom-x/internal/ech"
	"phantom-x/pkg/config"
	"phantom-x/pkg/metrics"
)

// ==================== WebSocket 连接封装 ====================

type WSConn struct {
	ID         int
	conn       *websocket.Conn
	writeCh    chan WriteJob
	closed     int32
	ctx        context.Context
	cancel     context.CancelFunc
	lastActive time.Time
	mu         sync.RWMutex
}

type WriteJob struct {
	Data     []byte
	Priority bool
	Done     chan error
}

func NewWSConn(id int, conn *websocket.Conn, queueSize int) *WSConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &WSConn{
		ID:         id,
		conn:       conn,
		writeCh:    make(chan WriteJob, queueSize),
		ctx:        ctx,
		cancel:     cancel,
		lastActive: time.Now(),
	}
}

func (w *WSConn) Send(data []byte, priority bool) error {
	if w.IsClosed() {
		return errors.New("connection closed")
	}

	job := WriteJob{
		Data:     data,
		Priority: priority,
		Done:     make(chan error, 1),
	}

	select {
	case w.writeCh <- job:
		return nil
	case <-time.After(10 * time.Second):
		metrics.IncrWriteTimeout()
		return errors.New("write timeout")
	case <-w.ctx.Done():
		return errors.New("connection closing")
	}
}

func (w *WSConn) SendSync(data []byte, timeout time.Duration) error {
	if w.IsClosed() {
		return errors.New("connection closed")
	}

	job := WriteJob{
		Data:     data,
		Priority: true,
		Done:     make(chan error, 1),
	}

	select {
	case w.writeCh <- job:
	case <-time.After(timeout):
		return errors.New("send timeout")
	}

	select {
	case err := <-job.Done:
		return err
	case <-time.After(timeout):
		return errors.New("write timeout")
	}
}

func (w *WSConn) Close() {
	if !atomic.CompareAndSwapInt32(&w.closed, 0, 1) {
		return
	}
	w.cancel()
	close(w.writeCh)
	if w.conn != nil {
		w.conn.Close()
	}
}

func (w *WSConn) IsClosed() bool {
	return atomic.LoadInt32(&w.closed) == 1
}

func (w *WSConn) UpdateActive() {
	w.mu.Lock()
	w.lastActive = time.Now()
	w.mu.Unlock()
}

func (w *WSConn) GetLastActive() time.Time {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastActive
}

func (w *WSConn) WriteMessage(msgType int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteMessage(msgType, data)
}

func (w *WSConn) ReadMessage() (int, []byte, error) {
	return w.conn.ReadMessage()
}

func (w *WSConn) SetReadDeadline(t time.Time) error {
	return w.conn.SetReadDeadline(t)
}

func (w *WSConn) SetWriteDeadline(t time.Time) error {
	return w.conn.SetWriteDeadline(t)
}

func (w *WSConn) SetPongHandler(h func(string) error) {
	w.conn.SetPongHandler(h)
}

func (w *WSConn) SetPingHandler(h func(string) error) {
	w.conn.SetPingHandler(h)
}

// ==================== WebSocket 拨号器 ====================

type Dialer struct {
	cfg *config.ClientConfig
}

func NewDialer(cfg *config.ClientConfig) *Dialer {
	return &Dialer{cfg: cfg}
}

func (d *Dialer) Dial(serverURL string, clientID string) (*websocket.Conn, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("id", clientID)
	u.RawQuery = q.Encode()

	// 构建 TLS 配置
	var tlsConfig *tls.Config
	if d.cfg.EnableECH && !d.cfg.Insecure {
		tlsConfig, err = ech.BuildTLSConfig(u.Hostname(), d.cfg.Insecure)
		if err != nil {
			// 降级到普通 TLS
			tlsConfig = &tls.Config{
				MinVersion:         tls.VersionTLS13,
				ServerName:         u.Hostname(),
				InsecureSkipVerify: d.cfg.Insecure,
			}
		}
	} else {
		tlsConfig = &tls.Config{
			MinVersion:         tls.VersionTLS13,
			ServerName:         u.Hostname(),
			InsecureSkipVerify: d.cfg.Insecure,
		}
	}

	dialer := websocket.Dialer{
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   64 * 1024,
		WriteBufferSize:  64 * 1024,
	}

	if d.cfg.Token != "" {
		dialer.Subprotocols = []string{d.cfg.Token}
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

// ==================== WebSocket 升级器 (服务端) ====================

type Upgrader struct {
	cfg      *config.ServerConfig
	upgrader websocket.Upgrader
}

func NewUpgrader(cfg *config.ServerConfig) *Upgrader {
	return &Upgrader{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
}

func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, string, error) {
	// Token 验证
	if u.cfg.Token != "" {
		protocols := websocket.Subprotocols(r)
		matched := false
		for _, p := range protocols {
			if p == u.cfg.Token {
				matched = true
				break
			}
		}
		if !matched {
			return nil, "", errors.New("unauthorized")
		}
		u.upgrader.Subprotocols = []string{u.cfg.Token}
	}

	conn, err := u.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, "", err
	}

	clientID := r.URL.Query().Get("id")
	if clientID == "" {
		clientID = fmt.Sprintf("anon-%s", r.RemoteAddr)
	}

	return conn, clientID, nil
}

// ==================== 工具函数 ====================

func IsNormalClose(err error) bool {
	if err == nil {
		return false
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code == websocket.CloseNormalClosure ||
			ce.Code == websocket.CloseGoingAway ||
			ce.Code == websocket.CloseNoStatusReceived
	}
	return errors.Is(err, net.ErrClosed)
}
