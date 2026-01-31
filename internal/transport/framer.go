package transport

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"phantom-x/internal/proto"
)

// ==================== 帧读取器 ====================

type FrameReader struct {
	r          io.Reader
	headerBuf  []byte
	payloadBuf []byte
	mu         sync.Mutex
}

func NewFrameReader(r io.Reader, maxPayload int) *FrameReader {
	return &FrameReader{
		r:          r,
		headerBuf:  make([]byte, proto.HeaderLen),
		payloadBuf: make([]byte, maxPayload),
	}
}

func (fr *FrameReader) ReadFrame() (*proto.Frame, error) {
	fr.mu.Lock()
	defer fr.mu.Unlock()

	// 读取头部
	if _, err := io.ReadFull(fr.r, fr.headerBuf); err != nil {
		return nil, err
	}

	cmd, streamID, flags, length := proto.UnpackHeader(fr.headerBuf)

	if length > len(fr.payloadBuf) {
		return nil, errors.New("payload too large")
	}

	var payload []byte
	if length > 0 {
		payload = fr.payloadBuf[:length]
		if _, err := io.ReadFull(fr.r, payload); err != nil {
			return nil, err
		}
	}

	// 处理 Padding
	if flags&proto.FlagPadding != 0 && len(payload) > 0 {
		// 去除填充
		payload = proto.RemovePadding(payload)
	}

	return &proto.Frame{
		Cmd:      cmd,
		StreamID: streamID,
		Flags:    flags,
		Payload:  payload,
	}, nil
}

// ==================== 帧写入器 ====================

type FrameWriter struct {
	w   io.Writer
	buf []byte
	mu  sync.Mutex
}

func NewFrameWriter(w io.Writer, bufSize int) *FrameWriter {
	return &FrameWriter{
		w:   w,
		buf: make([]byte, bufSize),
	}
}

func (fw *FrameWriter) WriteFrame(frame *proto.Frame) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	n := proto.PackFrame(fw.buf, frame.Cmd, frame.StreamID, frame.Flags, frame.Payload, 0)
	_, err := fw.w.Write(fw.buf[:n])
	return err
}

func (fw *FrameWriter) WriteFrameWithPadding(frame *proto.Frame, paddingCalc *proto.PaddingCalculator) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	n := proto.PackFrameWithPadding(fw.buf, frame.Cmd, frame.StreamID, frame.Flags, frame.Payload, paddingCalc)
	_, err := fw.w.Write(fw.buf[:n])
	return err
}

// ==================== 缓冲区池 ====================

var (
	SmallBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, 4*1024)
			return &b
		},
	}
	MediumBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, 32*1024)
			return &b
		},
	}
	LargeBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, 64*1024)
			return &b
		},
	}
)

func GetSmallBuf() *[]byte  { return SmallBufPool.Get().(*[]byte) }
func PutSmallBuf(b *[]byte) { SmallBufPool.Put(b) }

func GetMediumBuf() *[]byte  { return MediumBufPool.Get().(*[]byte) }
func PutMediumBuf(b *[]byte) { MediumBufPool.Put(b) }

func GetLargeBuf() *[]byte  { return LargeBufPool.Get().(*[]byte) }
func PutLargeBuf(b *[]byte) { LargeBufPool.Put(b) }

// ==================== 编解码辅助 ====================

func EncodeUint16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func DecodeUint16(b []byte) uint16 {
	return binary.BigEndian.Uint16(b)
}

func EncodeUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func DecodeUint32(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}
