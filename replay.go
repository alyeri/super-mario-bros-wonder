package main

// replay — Generic gRPC hybrid codec and unknown RPC logger/recorder.
// Allows capturing and logging raw Protobuf payloads of every incoming call made by Wonder.

import (
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type rawMsg struct{ b []byte }

type hybridCodec struct{}

func (hybridCodec) Marshal(v any) ([]byte, error) {
	if r, ok := v.(*rawMsg); ok {
		payload := append([]byte(nil), r.b...)
		wonderProbe.captureWire("out", messageTypeName(v), payload)
		return payload, nil
	}
	if m, ok := v.(proto.Message); ok {
		payload, err := proto.Marshal(m)
		if err == nil {
			wonderProbe.captureWire("out", messageTypeName(v), payload)
		}
		return payload, err
	}
	return nil, fmt.Errorf("hybridCodec: cannot marshal %T", v)
}

func (hybridCodec) Unmarshal(data []byte, v any) error {
	wonderProbe.captureWire("in", messageTypeName(v), append([]byte(nil), data...))
	if r, ok := v.(*rawMsg); ok {
		r.b = append([]byte(nil), data...)
		return nil
	}
	if m, ok := v.(proto.Message); ok {
		err := proto.Unmarshal(data, m)
		if err != nil {
			wonderProbe.record(probeEvent{
				Kind:        "grpc_decode_error",
				Direction:   "in",
				MessageType: messageTypeName(v),
				Error:       err.Error(),
				Fields: map[string]interface{}{
					"payload_length": len(data),
				},
			})
		}
		return err
	}
	return fmt.Errorf("hybridCodec: cannot unmarshal into %T", v)
}

func (hybridCodec) Name() string { return "proto" }

func newHybridCodec() hybridCodec { return hybridCodec{} }

func replayHandler(srv any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	log.Printf("[NPLN PROBE UNHANDLED] Incoming method: %s", method)

	var in rawMsg
	if err := stream.RecvMsg(&in); err != nil {
		log.Printf("[NPLN PROBE] RecvMsg failed on %s: %v", method, err)
		return err
	}

	if envOr("NPLN_UNKNOWN_RPC_MODE", "unimplemented") == "empty" {
		log.Printf("[NPLN PROBE UNHANDLED] %s -> configured empty response", method)
		return stream.SendMsg(&rawMsg{b: []byte{}})
	}
	return status.Errorf(codes.Unimplemented, "captured unknown RPC %s", method)
}
