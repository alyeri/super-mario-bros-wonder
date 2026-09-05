package main

// observability records the evidence needed to evaluate a Wonder connection
// without relying on the server's human-readable console log alone.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type probeContextKey uint8

const (
	probeConnectionKey probeContextKey = iota
	probeRemoteKey
	probeRPCKey
	probeMethodKey
)

type probeEvent struct {
	Timestamp        time.Time              `json:"timestamp"`
	Sequence         uint64                 `json:"sequence"`
	Kind             string                 `json:"kind"`
	ConnectionID     string                 `json:"connection_id,omitempty"`
	RPCID            string                 `json:"rpc_id,omitempty"`
	Method           string                 `json:"method,omitempty"`
	Direction        string                 `json:"direction,omitempty"`
	MessageType      string                 `json:"message_type,omitempty"`
	Metadata         map[string][]string    `json:"metadata,omitempty"`
	PayloadFile      string                 `json:"payload_file,omitempty"`
	PayloadJSONFile  string                 `json:"payload_json_file,omitempty"`
	PayloadSHA256    string                 `json:"payload_sha256,omitempty"`
	PayloadLength    int                    `json:"payload_length,omitempty"`
	CompressedLength int                    `json:"compressed_length,omitempty"`
	WireLength       int                    `json:"wire_length,omitempty"`
	StatusCode       string                 `json:"status_code,omitempty"`
	Error            string                 `json:"error,omitempty"`
	Fields           map[string]interface{} `json:"fields,omitempty"`
}

type probeRecorder struct {
	dir             string
	eventsPath      string
	mu              sync.Mutex
	eventSequence   atomic.Uint64
	payloadSequence atomic.Uint64
	connectionSeq   atomic.Uint64
	rpcSequence     atomic.Uint64
	connections     sync.Map
}

var wonderProbe = newProbeRecorder()

func newProbeRecorder() *probeRecorder {
	dir := envOr("WONDER_PROBE_DIR", "runtime/logs")
	return &probeRecorder{
		dir:        dir,
		eventsPath: filepath.Join(dir, "npln_events.jsonl"),
	}
}

func (r *probeRecorder) record(event probeEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.Sequence = r.eventSequence.Add(1)

	line, err := json.Marshal(event)
	if err != nil {
		log.Printf("[NPLN PROBE] cannot encode %s event: %v", event.Kind, err)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		log.Printf("[NPLN PROBE] cannot create %s: %v", r.dir, err)
		return
	}
	f, err := os.OpenFile(r.eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[NPLN PROBE] cannot open event log: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Printf("[NPLN PROBE] cannot append event: %v", err)
	}
}

func (r *probeRecorder) registerTLSConnection(remote, local net.Addr) string {
	id := fmt.Sprintf("conn-%06d", r.connectionSeq.Add(1))
	remoteText := addressString(remote)
	if remoteText != "" {
		r.connections.Store(remoteText, id)
	}
	r.record(probeEvent{
		Kind:         "tcp_accepted",
		ConnectionID: id,
		Fields: map[string]interface{}{
			"remote": remoteText,
			"local":  addressString(local),
		},
	})
	return id
}

func (r *probeRecorder) connectionID(remote net.Addr) string {
	remoteText := addressString(remote)
	if id, ok := r.connections.Load(remoteText); ok {
		return id.(string)
	}
	id := fmt.Sprintf("conn-%06d", r.connectionSeq.Add(1))
	if remoteText != "" {
		r.connections.Store(remoteText, id)
	}
	return id
}

func (r *probeRecorder) forgetConnection(remote string) {
	if remote != "" {
		r.connections.Delete(remote)
	}
}

func (r *probeRecorder) capturePayload(event probeEvent, payload []byte, jsonPayload []byte) {
	sum := sha256.Sum256(payload)
	event.PayloadLength = len(payload)
	event.PayloadSHA256 = hex.EncodeToString(sum[:])

	sequence := r.payloadSequence.Add(1)
	label := event.Method
	if label == "" {
		label = event.MessageType
	}
	base := fmt.Sprintf("%06d_%s_%s", sequence, sanitizeCaptureName(event.Direction), sanitizeCaptureName(label))
	payloadDir := filepath.Join(r.dir, "payloads")
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		event.Error = joinProbeError(event.Error, fmt.Sprintf("create payload directory: %v", err))
		r.record(event)
		return
	}

	binPath := filepath.Join(payloadDir, base+".bin")
	if err := os.WriteFile(binPath, payload, 0o644); err != nil {
		event.Error = joinProbeError(event.Error, fmt.Sprintf("write payload: %v", err))
	} else {
		event.PayloadFile = relativeCapturePath(r.dir, binPath)
	}

	if len(jsonPayload) > 0 {
		jsonPath := filepath.Join(payloadDir, base+".json")
		if err := os.WriteFile(jsonPath, jsonPayload, 0o644); err != nil {
			event.Error = joinProbeError(event.Error, fmt.Sprintf("write JSON payload: %v", err))
		} else {
			event.PayloadJSONFile = relativeCapturePath(r.dir, jsonPath)
		}
	}

	if event.Direction == "in" && event.Method != "" {
		r.appendProbeIndex(event)
	}
	r.record(event)
}

func (r *probeRecorder) appendProbeIndex(event probeEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		log.Printf("[NPLN PROBE] cannot create %s: %v", r.dir, err)
		return
	}
	path := filepath.Join(r.dir, "wonder_rpc_probe.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[NPLN PROBE] cannot open RPC index: %v", err)
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(
		f,
		"%s method=%s type=%s length=%d sha256=%s payload=%s\n",
		time.Now().UTC().Format(time.RFC3339Nano),
		event.Method,
		event.MessageType,
		event.PayloadLength,
		event.PayloadSHA256,
		event.PayloadFile,
	)
}

func (r *probeRecorder) captureWire(direction, messageType string, payload []byte) {
	r.capturePayload(probeEvent{
		Kind:        "grpc_wire_message",
		Direction:   direction,
		MessageType: messageType,
	}, payload, nil)
}

func (r *probeRecorder) captureMessage(ctx context.Context, direction, method string, message any) {
	event := eventFromContext(ctx, "grpc_message")
	event.Direction = direction
	event.Method = method
	event.MessageType = messageTypeName(message)

	if raw, ok := message.(*rawMsg); ok {
		r.capturePayload(event, append([]byte(nil), raw.b...), nil)
		return
	}

	protoMessage, ok := message.(proto.Message)
	if !ok {
		event.Error = fmt.Sprintf("cannot serialize %T", message)
		r.record(event)
		return
	}

	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(protoMessage)
	if err != nil {
		event.Error = fmt.Sprintf("protobuf marshal: %v", err)
		r.record(event)
		return
	}
	jsonPayload, jsonErr := (protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: false,
		Indent:          "  ",
	}).Marshal(protoMessage)
	if jsonErr != nil {
		event.Error = fmt.Sprintf("protobuf JSON marshal: %v", jsonErr)
		// FieldMask("*") is accepted on the wire but rejected by protojson.
		// Keep a labelled diagnostic fallback without changing the protobuf bytes.
		jsonPayload, _ = json.MarshalIndent(protoMessage, "", "  ")
		if event.Fields == nil {
			event.Fields = map[string]interface{}{}
		}
		event.Fields["json_encoding"] = "go-protobuf-fallback"
	}
	r.capturePayload(event, payload, jsonPayload)
}

func (r *probeRecorder) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	id := r.connectionID(info.RemoteAddr)
	ctx = context.WithValue(ctx, probeConnectionKey, id)
	ctx = context.WithValue(ctx, probeRemoteKey, addressString(info.RemoteAddr))
	r.record(probeEvent{
		Kind:         "grpc_transport_tagged",
		ConnectionID: id,
		Fields: map[string]interface{}{
			"remote": addressString(info.RemoteAddr),
			"local":  addressString(info.LocalAddr),
		},
	})
	return ctx
}

func (r *probeRecorder) HandleConn(ctx context.Context, event stats.ConnStats) {
	connID, _ := ctx.Value(probeConnectionKey).(string)
	switch event.(type) {
	case *stats.ConnBegin:
		r.record(probeEvent{Kind: "grpc_transport_begin", ConnectionID: connID})
	case *stats.ConnEnd:
		r.record(probeEvent{Kind: "grpc_transport_end", ConnectionID: connID})
		remote, _ := ctx.Value(probeRemoteKey).(string)
		r.forgetConnection(remote)
	}
}

func (r *probeRecorder) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	rpcID := fmt.Sprintf("rpc-%06d", r.rpcSequence.Add(1))
	ctx = context.WithValue(ctx, probeRPCKey, rpcID)
	ctx = context.WithValue(ctx, probeMethodKey, info.FullMethodName)
	r.record(eventFromContext(ctx, "grpc_rpc_tagged"))
	return ctx
}

func (r *probeRecorder) HandleRPC(ctx context.Context, statEvent stats.RPCStats) {
	event := eventFromContext(ctx, "")
	switch value := statEvent.(type) {
	case *stats.InHeader:
		event.Kind = "grpc_in_headers"
		event.Method = value.FullMethod
		event.Metadata = captureMetadata(value.Header)
		event.WireLength = value.WireLength
		event.Fields = map[string]interface{}{
			"compression": value.Compression,
			"remote":      addressString(value.RemoteAddr),
			"local":       addressString(value.LocalAddr),
		}
	case *stats.Begin:
		event.Kind = "grpc_rpc_begin"
		event.Timestamp = value.BeginTime.UTC()
		event.Fields = map[string]interface{}{
			"client_stream": value.IsClientStream,
			"server_stream": value.IsServerStream,
		}
	case *stats.InPayload:
		event.Kind = "grpc_in_payload"
		event.Direction = "in"
		event.MessageType = messageTypeName(value.Payload)
		event.PayloadLength = value.Length
		event.CompressedLength = value.CompressedLength
		event.WireLength = value.WireLength
	case *stats.OutPayload:
		event.Kind = "grpc_out_payload"
		event.Direction = "out"
		event.MessageType = messageTypeName(value.Payload)
		event.PayloadLength = value.Length
		event.CompressedLength = value.CompressedLength
		event.WireLength = value.WireLength
	case *stats.OutHeader:
		event.Kind = "grpc_out_headers"
		event.Metadata = captureMetadata(value.Header)
		event.Fields = map[string]interface{}{"compression": value.Compression}
	case *stats.OutTrailer:
		event.Kind = "grpc_out_trailers"
		event.Metadata = captureMetadata(value.Trailer)
	case *stats.End:
		event.Kind = "grpc_rpc_end"
		event.Timestamp = value.EndTime.UTC()
		event.StatusCode = status.Code(value.Error).String()
		event.Fields = map[string]interface{}{
			"duration_ms": value.EndTime.Sub(value.BeginTime).Milliseconds(),
		}
		if value.Error != nil {
			event.Error = value.Error.Error()
		}
	default:
		return
	}
	r.record(event)
}

func traceStreamInterceptor(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	logMetadata(stream.Context(), info.FullMethod)
	wrapped := &observedServerStream{
		ServerStream: stream,
		method:       info.FullMethod,
	}
	err := handler(srv, wrapped)
	logRPCResult(info.FullMethod, err)
	return err
}

type observedServerStream struct {
	grpc.ServerStream
	method string
}

func (s *observedServerStream) RecvMsg(message any) error {
	err := s.ServerStream.RecvMsg(message)
	if err == nil {
		wonderProbe.captureMessage(s.Context(), "in", s.method, message)
	} else if err != io.EOF {
		event := eventFromContext(s.Context(), "grpc_stream_receive_error")
		event.Method = s.method
		event.Error = err.Error()
		wonderProbe.record(event)
	}
	return err
}

func (s *observedServerStream) SendMsg(message any) error {
	err := s.ServerStream.SendMsg(message)
	if err == nil {
		wonderProbe.captureMessage(s.Context(), "out", s.method, message)
	} else {
		event := eventFromContext(s.Context(), "grpc_stream_send_error")
		event.Method = s.method
		event.Error = err.Error()
		wonderProbe.record(event)
	}
	return err
}

func logRPCResult(method string, err error) {
	if err != nil {
		log.Printf("[NPLN RPC ERROR] %s -> code=%s error=%v", method, status.Code(err), err)
		return
	}
	log.Printf("[NPLN RPC OK] %s", method)
}

func eventFromContext(ctx context.Context, kind string) probeEvent {
	connID, _ := ctx.Value(probeConnectionKey).(string)
	rpcID, _ := ctx.Value(probeRPCKey).(string)
	method, _ := ctx.Value(probeMethodKey).(string)
	return probeEvent{
		Kind:         kind,
		ConnectionID: connID,
		RPCID:        rpcID,
		Method:       method,
	}
}

func captureMetadata(md metadata.MD) map[string][]string {
	if len(md) == 0 {
		return nil
	}
	out := make(map[string][]string, len(md))
	for key, values := range md {
		normalizedKey := strings.ToLower(key)
		captured := make([]string, 0, len(values))
		for _, value := range values {
			switch {
			case isSensitiveMetadata(normalizedKey):
				captured = append(captured, redactMetadataValue(value))
			case strings.HasSuffix(normalizedKey, "-bin") || !utf8.ValidString(value):
				captured = append(captured, "base64:"+base64.StdEncoding.EncodeToString([]byte(value)))
			default:
				captured = append(captured, value)
			}
		}
		out[normalizedKey] = captured
	}
	return out
}

func redactMetadataValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("<redacted length=%d sha256=%x>", len(value), sum[:8])
}

func isSensitiveMetadata(key string) bool {
	return key == "authorization" ||
		key == "proxy-authorization" ||
		key == "cookie" ||
		key == "set-cookie" ||
		strings.Contains(key, "token") ||
		strings.Contains(key, "secret")
}

func messageTypeName(message any) string {
	if message == nil {
		return "<nil>"
	}
	if protoMessage, ok := message.(proto.Message); ok {
		return string(protoMessage.ProtoReflect().Descriptor().FullName())
	}
	return fmt.Sprintf("%T", message)
}

func sanitizeCaptureName(value string) string {
	value = strings.Trim(value, "/")
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

func relativeCapturePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}

func joinProbeError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func addressString(address net.Addr) string {
	if address == nil {
		return ""
	}
	return address.String()
}

func sortedServiceManifest(server *grpc.Server) []map[string]interface{} {
	services := server.GetServiceInfo()
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		info := services[name]
		methods := make([]map[string]interface{}, 0, len(info.Methods))
		for _, method := range info.Methods {
			methods = append(methods, map[string]interface{}{
				"name":          method.Name,
				"client_stream": method.IsClientStream,
				"server_stream": method.IsServerStream,
			})
		}
		sort.Slice(methods, func(i, j int) bool {
			return methods[i]["name"].(string) < methods[j]["name"].(string)
		})
		result = append(result, map[string]interface{}{
			"service": name,
			"methods": methods,
		})
	}
	return result
}
