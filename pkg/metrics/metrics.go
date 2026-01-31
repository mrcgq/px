package metrics

import (
	"fmt"
	"sync/atomic"
	"time"
)

// 全局指标
var (
	packetsSent   int64
	packetsRecv   int64
	bytesSent     int64
	bytesRecv     int64
	activeStreams int64
	writeTimeouts int64
	connectErrors int64
	startTime     = time.Now()
)

func IncrPacketsSent(n int64) { atomic.AddInt64(&packetsSent, n) }
func IncrPacketsRecv(n int64) { atomic.AddInt64(&packetsRecv, n) }
func AddBytesSent(n int64)    { atomic.AddInt64(&bytesSent, n) }
func AddBytesRecv(n int64)    { atomic.AddInt64(&bytesRecv, n) }
func IncrActiveStreams()      { atomic.AddInt64(&activeStreams, 1) }
func DecrActiveStreams()      { atomic.AddInt64(&activeStreams, -1) }
func IncrWriteTimeout()       { atomic.AddInt64(&writeTimeouts, 1) }
func IncrConnectError()       { atomic.AddInt64(&connectErrors, 1) }

// Stats 获取统计信息
type Stats struct {
	Uptime        time.Duration
	PacketsSent   int64
	PacketsRecv   int64
	BytesSent     int64
	BytesRecv     int64
	ActiveStreams int64
	WriteTimeouts int64
	ConnectErrors int64
}

func GetStats() Stats {
	return Stats{
		Uptime:        time.Since(startTime),
		PacketsSent:   atomic.LoadInt64(&packetsSent),
		PacketsRecv:   atomic.LoadInt64(&packetsRecv),
		BytesSent:     atomic.LoadInt64(&bytesSent),
		BytesRecv:     atomic.LoadInt64(&bytesRecv),
		ActiveStreams: atomic.LoadInt64(&activeStreams),
		WriteTimeouts: atomic.LoadInt64(&writeTimeouts),
		ConnectErrors: atomic.LoadInt64(&connectErrors),
	}
}

func (s Stats) String() string {
	return fmt.Sprintf(
		"Uptime: %v | Streams: %d | TX: %d pkts/%s | RX: %d pkts/%s | Errors: %d",
		s.Uptime.Round(time.Second),
		s.ActiveStreams,
		s.PacketsSent, formatBytes(s.BytesSent),
		s.PacketsRecv, formatBytes(s.BytesRecv),
		s.WriteTimeouts+s.ConnectErrors,
	)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
