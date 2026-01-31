package pool

import (
	"phantom-x/internal/proto"
	// ... 其他 import
)

// Config 增加 Padding 配置
type Config struct {
	// ... 现有配置 ...
	
	// Padding 配置
	EnablePadding     bool   `yaml:"enable_padding"`
	PaddingMinSize    int    `yaml:"padding_min_size"`
	PaddingMaxSize    int    `yaml:"padding_max_size"`
	PaddingDistribute string `yaml:"padding_distribution"` // uniform, normal, mimicry
}

// WSConn 增加 Padding 计算器
type WSConn struct {
	// ... 现有字段 ...
	
	paddingCalc *proto.PaddingCalculator
}

// 初始化时创建 Padding 计算器
func (p *ConnPool) maintainConnection(id int) {
	// ... 现有代码 ...
	
	// 创建 Padding 计算器
	var paddingCalc *proto.PaddingCalculator
	if p.cfg.EnablePadding {
		paddingCalc = proto.NewPaddingCalculator(&proto.PaddingConfig{
			Enabled:      true,
			MinSize:      p.cfg.PaddingMinSize,
			MaxPadding:   p.cfg.PaddingMaxSize,
			Distribution: p.cfg.PaddingDistribute,
		})
	}
	
	wsConn := &WSConn{
		// ... 现有字段 ...
		paddingCalc: paddingCalc,
	}
	
	// ... 其余代码 ...
}

// 修改后的 writeLoop
func (w *WSConn) writeLoop() {
	defer w.Close()
	
	const maxAggSize = 64 * 1024
	const aggDelay = 5 * time.Millisecond
	
	pendingData := make(map[uint32][]byte)
	flushTimer := time.NewTimer(aggDelay)
	defer flushTimer.Stop()
	
	// 发送缓冲区
	sendBuf := make([]byte, maxAggSize+proto.MaxPadding+proto.HeaderLen)
	
	flush := func() {
		if len(pendingData) == 0 {
			return
		}
		
		// 构建聚合包
		agg := &proto.AggregatedData{}
		for sid, data := range pendingData {
			agg.Items = append(agg.Items, struct {
				StreamID uint32
				Data     []byte
			}{sid, data})
		}
		
		var frameLen int
		
		if len(agg.Items) == 1 {
			// 单个流，直接发送（带 Padding）
			item := agg.Items[0]
			frameLen = proto.PackFrameWithPadding(
				sendBuf,
				proto.CmdData,
				item.StreamID,
				0, // flags
				item.Data,
				w.paddingCalc,
			)
		} else {
			// 多个流，发送聚合包（带 Padding）
			aggData := agg.Encode()
			frameLen = proto.PackFrameWithPadding(
				sendBuf,
				proto.CmdData,
				0,
				proto.FlagAggregate,
				aggData,
				w.paddingCalc,
			)
		}
		
		// 发送
		w.conn.SetWriteDeadline(time.Now().Add(w.pool.cfg.WriteTimeout))
		if err := w.conn.WriteMessage(websocket.BinaryMessage, sendBuf[:frameLen]); err != nil {
			log.Printf("[Pool] Write error on conn %d: %v", w.ID, err)
			w.Close()
			return
		}
		
		metrics.IncrPacketsSent(int64(len(agg.Items)))
		metrics.AddBytesSent(int64(frameLen))
		
		// 清空缓冲
		pendingData = make(map[uint32][]byte)
	}
	
	for {
		select {
		case <-w.ctx.Done():
			flush()
			return
			
		case job, ok := <-w.writeCh:
			if !ok {
				flush()
				return
			}
			
			// 解析帧
			if len(job.Data) >= proto.HeaderLen {
				cmd, sid, flags, _ := proto.UnpackHeader(job.Data[:proto.HeaderLen])
				
				// 数据包且非优先级，进行聚合
				if cmd == proto.CmdData && !job.Priority && flags&proto.FlagAggregate == 0 {
					payload := job.Data[proto.HeaderLen:]
					if existing, ok := pendingData[sid]; ok {
						pendingData[sid] = append(existing, payload...)
					} else {
						pendingData[sid] = payload
					}
					
					// 检查大小限制
					totalSize := 0
					for _, d := range pendingData {
						totalSize += len(d)
					}
					if totalSize >= maxAggSize {
						flush()
					}
					continue
				}
			}
			
			// 非数据包或优先级包
			flush()
			
			// 对控制包也应用 Padding（可配置）
			if w.paddingCalc != nil && len(job.Data) >= proto.HeaderLen {
				cmd, sid, flags, length := proto.UnpackHeader(job.Data[:proto.HeaderLen])
				payload := job.Data[proto.HeaderLen : proto.HeaderLen+length]
				
				frameLen := proto.PackFrameWithPadding(
					sendBuf,
					cmd,
					sid,
					flags,
					payload,
					w.paddingCalc,
				)
				
				w.conn.SetWriteDeadline(time.Now().Add(w.pool.cfg.WriteTimeout))
				if err := w.conn.WriteMessage(websocket.BinaryMessage, sendBuf[:frameLen]); err != nil {
					log.Printf("[Pool] Write error on conn %d: %v", w.ID, err)
					return
				}
				metrics.IncrPacketsSent(1)
				metrics.AddBytesSent(int64(frameLen))
			} else {
				// 无 Padding，直接发送
				w.conn.SetWriteDeadline(time.Now().Add(w.pool.cfg.WriteTimeout))
				if err := w.conn.WriteMessage(websocket.BinaryMessage, job.Data); err != nil {
					return
				}
				metrics.IncrPacketsSent(1)
			}
			
		case <-flushTimer.C:
			flush()
			flushTimer.Reset(aggDelay)
		}
	}
}
