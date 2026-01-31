package proto

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	mrand "math/rand"
	"time"
)

// ==================== Padding 配置 ====================
type PaddingConfig struct {
	Enabled       bool    // 是否启用
	MinSize       int     // 最小包大小目标
	MaxPadding    int     // 最大填充长度
	Distribution  string  // 填充分布: "uniform", "normal", "mimicry"
	MimicryTarget string  // 模拟目标: "https", "websocket"
}

func DefaultPaddingConfig() *PaddingConfig {
	return &PaddingConfig{
		Enabled:       true,
		MinSize:       64,        // 最小 64 字节
		MaxPadding:    256,       // 最大填充 256 字节
		Distribution:  "mimicry", // 模拟 HTTPS 流量
		MimicryTarget: "https",
	}
}

// ==================== 智能 Padding 计算器 ====================
type PaddingCalculator struct {
	cfg  *PaddingConfig
	rng  *mrand.Rand
	
	// HTTPS 流量模拟参数（基于真实统计）
	httpsSmallPktProb  float64 // 小包概率
	httpsSmallPktMean  float64 // 小包均值
	httpsSmallPktStd   float64 // 小包标准差
	httpsLargePktMean  float64 // 大包均值
	httpsLargePktStd   float64 // 大包标准差
}

func NewPaddingCalculator(cfg *PaddingConfig) *PaddingCalculator {
	return &PaddingCalculator{
		cfg: cfg,
		rng: mrand.New(mrand.NewSource(time.Now().UnixNano())),
		// 基于真实 HTTPS 流量统计的参数
		httpsSmallPktProb: 0.3,   // 30% 是小包（ACK 等）
		httpsSmallPktMean: 60,    // 小包平均 60 字节
		httpsSmallPktStd:  20,    // 标准差 20
		httpsLargePktMean: 1200,  // 大包平均 1200 字节
		httpsLargePktStd:  400,   // 标准差 400
	}
}

// CalculatePadding 计算填充长度
func (p *PaddingCalculator) CalculatePadding(currentSize int) int {
	if !p.cfg.Enabled {
		return 0
	}
	
	switch p.cfg.Distribution {
	case "uniform":
		return p.uniformPadding(currentSize)
	case "normal":
		return p.normalPadding(currentSize)
	case "mimicry":
		return p.mimicryPadding(currentSize)
	default:
		return p.uniformPadding(currentSize)
	}
}

// 均匀分布填充
func (p *PaddingCalculator) uniformPadding(currentSize int) int {
	if currentSize >= p.cfg.MinSize {
		// 已经足够大，随机添加少量填充
		return p.rng.Intn(p.cfg.MaxPadding / 4)
	}
	
	// 填充到最小大小，再加随机量
	base := p.cfg.MinSize - currentSize
	extra := p.rng.Intn(p.cfg.MaxPadding / 2)
	
	padding := base + extra
	if padding > p.cfg.MaxPadding {
		padding = p.cfg.MaxPadding
	}
	return padding
}

// 正态分布填充
func (p *PaddingCalculator) normalPadding(currentSize int) int {
	// 目标大小 = 当前大小 + 正态分布随机数
	mean := float64(p.cfg.MaxPadding) / 2
	std := float64(p.cfg.MaxPadding) / 4
	
	padding := int(p.rng.NormFloat64()*std + mean)
	if padding < 0 {
		padding = 0
	}
	if padding > p.cfg.MaxPadding {
		padding = p.cfg.MaxPadding
	}
	return padding
}

// 模拟真实 HTTPS 流量的填充
func (p *PaddingCalculator) mimicryPadding(currentSize int) int {
	// 生成一个符合 HTTPS 流量分布的目标大小
	var targetSize int
	
	if p.rng.Float64() < p.httpsSmallPktProb {
		// 生成小包大小
		targetSize = int(p.rng.NormFloat64()*p.httpsSmallPktStd + p.httpsSmallPktMean)
	} else {
		// 生成大包大小
		targetSize = int(p.rng.NormFloat64()*p.httpsLargePktStd + p.httpsLargePktMean)
	}
	
	// 确保目标大小合理
	if targetSize < p.cfg.MinSize {
		targetSize = p.cfg.MinSize
	}
	if targetSize > 1500 { // MTU 限制
		targetSize = 1500
	}
	
	padding := targetSize - currentSize
	if padding < 0 {
		padding = 0
	}
	if padding > p.cfg.MaxPadding {
		padding = p.cfg.MaxPadding
	}
	
	return padding
}

// ==================== 帧编码增强 ====================

// PackFrameWithPadding 带智能填充的帧编码
func PackFrameWithPadding(buf []byte, cmd byte, streamID uint32, flags byte, payload []byte, paddingCalc *PaddingCalculator) int {
	currentSize := HeaderLen + len(payload)
	paddingLen := 0
	
	if paddingCalc != nil {
		paddingLen = paddingCalc.CalculatePadding(currentSize)
	}
	
	// 设置填充标志
	if paddingLen > 0 {
		flags |= FlagPadding
	}
	
	// 编码帧头
	buf[0] = cmd
	binary.BigEndian.PutUint32(buf[1:5], streamID)
	buf[5] = flags
	
	// 实际负载长度（不含填充长度字段）
	// 如果有填充，在 payload 后加 1 字节表示填充长度
	totalPayloadLen := len(payload)
	if paddingLen > 0 {
		totalPayloadLen += 1 + paddingLen // 1 字节长度 + 填充数据
	}
	
	binary.BigEndian.PutUint16(buf[6:8], uint16(totalPayloadLen))
	
	offset := HeaderLen
	
	// 写入实际数据
	if len(payload) > 0 {
		copy(buf[offset:], payload)
		offset += len(payload)
	}
	
	// 写入填充
	if paddingLen > 0 {
		buf[offset] = byte(paddingLen) // 填充长度
		offset++
		
		// 生成填充数据（半随机，降低熵值）
		generatePadding(buf[offset:offset+paddingLen], paddingLen)
		offset += paddingLen
	}
	
	return offset
}

// generatePadding 生成混合填充数据
// 策略：50% 随机 + 50% 类似 HTTP 的可见字符，降低熵值
func generatePadding(buf []byte, length int) {
	// HTTP 常见字符（降低熵值）
	httpChars := []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~:/?#[]@!$&'()*+,;= \r\n")
	
	halfLen := length / 2
	
	// 前半部分：随机字节
	rand.Read(buf[:halfLen])
	
	// 后半部分：类 HTTP 字符
	for i := halfLen; i < length; i++ {
		buf[i] = httpChars[mrand.Intn(len(httpChars))]
	}
}

// UnpackFrameWithPadding 解码带填充的帧
func UnpackFrameWithPadding(data []byte) (cmd byte, streamID uint32, flags byte, payload []byte, err error) {
	if len(data) < HeaderLen {
		return 0, 0, 0, nil, ErrFrameTooShort
	}
	
	cmd, streamID, flags, length := UnpackHeader(data[:HeaderLen])
	
	if len(data) < HeaderLen+length {
		return 0, 0, 0, nil, ErrInvalidLength
	}
	
	rawPayload := data[HeaderLen : HeaderLen+length]
	
	// 检查是否有填充
	if flags&FlagPadding != 0 && len(rawPayload) > 0 {
		// 最后一部分是填充：[实际数据][填充长度:1][填充数据:N]
		// 从末尾解析填充长度
		paddingLen := int(rawPayload[len(rawPayload)-1])
		
		// 验证填充长度合理性
		if paddingLen+1 > len(rawPayload) {
			// 填充长度不合理，可能是旧格式或错误
			payload = rawPayload
		} else {
			// 提取实际数据（去掉填充长度字段和填充数据）
			// 注意：填充长度字段在实际数据之后
			actualEnd := len(rawPayload) - 1 - paddingLen
			if actualEnd > 0 {
				payload = rawPayload[:actualEnd]
			}
		}
	} else {
		payload = rawPayload
	}
	
	return cmd, streamID, flags, payload, nil
}

// 错误定义
var (
	ErrFrameTooShort = errors.New("frame too short")
	ErrInvalidLength = errors.New("invalid length")
)
