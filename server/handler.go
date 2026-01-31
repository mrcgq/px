// server/handler.go

func (s *Session) handleFrame(data []byte) {
	// 使用带 Padding 支持的解码
	cmd, streamID, flags, payload, err := proto.UnpackFrameWithPadding(data)
	if err != nil {
		return
	}
	
	// 处理聚合包
	if cmd == proto.CmdData && flags&proto.FlagAggregate != 0 {
		agg, err := proto.DecodeAggregatedData(payload)
		if err != nil {
			return
		}
		for _, item := range agg.Items {
			s.handleStreamData(item.StreamID, item.Data)
		}
		return
	}
	
	// 处理普通包
	switch cmd {
	case proto.CmdOpenTCP:
		go s.handleTCPOpen(streamID, payload)
	case proto.CmdData:
		s.handleStreamData(streamID, payload)
	case proto.CmdClose:
		s.closeStream(streamID)
	}
}
