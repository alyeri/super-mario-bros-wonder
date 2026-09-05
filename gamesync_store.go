package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	commonpb "npln.nintendo.net/npln-practice/proto/common"
	gspb "npln.nintendo.net/npln-practice/proto/gamesync/v1"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
)

func cloneFields(fields *commonpb.MapValue) *commonpb.MapValue {
	if fields == nil {
		return &commonpb.MapValue{Fields: map[string]*commonpb.Value{}}
	}
	out := proto.Clone(fields).(*commonpb.MapValue)
	if out.Fields == nil {
		out.Fields = map[string]*commonpb.Value{}
	}
	return out
}

// The six keys/types below are read by Wonder 1.0.0 at flat offset 0x156144.
// Seed length is a local policy, not established by that parser. Both peers
// receive the same cryptographically random bytes for the life of the session.
func (g *gamesyncServer) initializeFixedData(gs *mmpb.GameSession) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.documents == nil {
		g.documents = make(map[string]map[string]*gspb.Document)
	}
	if g.documents[gs.Name] == nil {
		g.documents[gs.Name] = make(map[string]*gspb.Document)
	}
	if g.documents[gs.Name]["docs/__gs/f"] != nil {
		return nil
	}
	seedSize := envInt("NPLN_GAMESYNC_SEED_BYTES", 16)
	if seedSize < 16 || seedSize > 64 {
		return fmt.Errorf("seed length outside local policy")
	}
	seed := make([]byte, seedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	now := timestamppb.Now()
	g.documents[gs.Name]["docs/__gs/f"] = &gspb.Document{Name: "docs/__gs/f", CreateTime: now, UpdateTime: now, Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
		"gsid": gamesyncStringValue(lastResourceSegment(gs.Name)), "addr": gamesyncStringValue(gs.Host), "p": gamesyncIntegerValue(int64(gs.Port)),
		"mcn": gamesyncStringValue(gs.Properties.GetFields()["_BaseConfigName"].GetStringValue()), "maxu": gamesyncIntegerValue(int64(gs.MaxParticipantCount)),
		"rs": {ValueType: &commonpb.Value_BytesValue{BytesValue: seed}},
	}}}
	return nil
}

func decodeGamesyncAttributes(data string) (*commonpb.MapValue, error) {
	out := cloneFields(nil)
	if data == "" {
		return out, nil
	}
	var attrs map[string]struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(data), &attrs); err != nil {
		return nil, err
	}
	for k, v := range attrs {
		switch v.Type {
		case "string":
			var x string
			if err := json.Unmarshal(v.Value, &x); err != nil {
				return nil, err
			}
			out.Fields[k] = gamesyncStringValue(x)
		case "integer":
			var x int64
			if err := json.Unmarshal(v.Value, &x); err != nil {
				return nil, err
			}
			out.Fields[k] = gamesyncIntegerValue(x)
		case "boolean":
			var x bool
			if err := json.Unmarshal(v.Value, &x); err != nil {
				return nil, err
			}
			out.Fields[k] = &commonpb.Value{ValueType: &commonpb.Value_BooleanValue{BooleanValue: x}}
		case "double":
			var x float64
			if err := json.Unmarshal(v.Value, &x); err != nil {
				return nil, err
			}
			out.Fields[k] = &commonpb.Value{ValueType: &commonpb.Value_DoubleValue{DoubleValue: x}}
		default:
			return nil, fmt.Errorf("unsupported signed attribute type %q", v.Type)
		}
	}
	return out, nil
}

func documentPath(name string) (string, string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "docs" || parts[1] == "" || parts[2] == "" || parts[2] == "." || parts[2] == ".." {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// Parse the documented dot-separated field path with backtick quoting. The
// wildcard belongs to a field mask, not to an individual field transform.
func fieldPath(path string) ([]string, error) {
	var parts []string
	var part strings.Builder
	quoted, escaped := false, false
	for _, c := range path {
		if escaped {
			part.WriteRune(c)
			escaped = false
			continue
		}
		if c == '\\' && quoted {
			escaped = true
			continue
		}
		if c == '`' {
			quoted = !quoted
			continue
		}
		if c == '.' && !quoted {
			if part.Len() == 0 {
				return nil, status.Error(codes.InvalidArgument, "empty field path")
			}
			parts = append(parts, part.String())
			part.Reset()
		} else {
			part.WriteRune(c)
		}
	}
	if quoted || escaped || part.Len() == 0 || path == "*" {
		return nil, status.Error(codes.InvalidArgument, "invalid field path")
	}
	return append(parts, part.String()), nil
}
func readField(fields *commonpb.MapValue, path []string) *commonpb.Value {
	for i, k := range path {
		v := fields.GetFields()[k]
		if i == len(path)-1 {
			return v
		}
		fields = v.GetMapValue()
	}
	return nil
}
func setField(fields *commonpb.MapValue, path []string, value *commonpb.Value) {
	for i, k := range path {
		if i == len(path)-1 {
			if value == nil {
				delete(fields.Fields, k)
			} else {
				fields.Fields[k] = proto.Clone(value).(*commonpb.Value)
			}
			return
		}
		next := fields.GetFields()[k].GetMapValue()
		if next == nil {
			next = cloneFields(nil)
			fields.Fields[k] = gamesyncMapValue(next)
		}
		if next.Fields == nil {
			next.Fields = map[string]*commonpb.Value{}
		}
		fields = next
	}
}
func maskedDocument(document *gspb.Document, mask *fieldmaskpb.FieldMask) (*gspb.Document, error) {
	out := proto.Clone(document).(*gspb.Document)
	if mask == nil || len(mask.Paths) == 1 && mask.Paths[0] == "*" {
		return out, nil
	}
	out.Fields = cloneFields(nil)
	for _, p := range mask.Paths {
		path, err := fieldPath(p)
		if err != nil {
			return nil, err
		}
		setField(out.Fields, path, readField(document.Fields, path))
	}
	return out, nil
}

func (g *gamesyncServer) GetDocument(ctx context.Context, req *gspb.GetDocumentRequest) (*gspb.Document, error) {
	session, err := g.authorizedSession(ctx, "")
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid session authorization")
	}
	if _, _, ok := documentPath(req.Name); !ok {
		return nil, status.Error(codes.InvalidArgument, "invalid document name")
	}
	if len(req.GetTransaction()) != 0 {
		return nil, status.Error(codes.Unimplemented, "transactional reads not supported")
	}
	g.mu.RLock()
	document := g.documents[session.GameSession][req.Name]
	if document != nil {
		document = proto.Clone(document).(*gspb.Document)
	}
	g.mu.RUnlock()
	if document == nil {
		return nil, status.Error(codes.NotFound, "document does not exist in this game session")
	}
	return maskedDocument(document, req.ReadMask)
}

func checkPrecondition(p *gspb.Precondition, doc *gspb.Document) error {
	if p == nil {
		return nil
	}
	switch x := p.GetConditionType().(type) {
	case *gspb.Precondition_Exists:
		if x.Exists != (doc != nil) {
			return status.Error(codes.FailedPrecondition, "document existence precondition failed")
		}
	case *gspb.Precondition_UpdateTime:
		if doc == nil || x.UpdateTime == nil || !proto.Equal(x.UpdateTime, doc.UpdateTime) {
			return status.Error(codes.FailedPrecondition, "document update time precondition failed")
		}
	default:
		return status.Error(codes.InvalidArgument, "empty precondition")
	}
	return nil
}

func writableDocument(name string, fields *commonpb.MapValue, session gamesyncSession, owners map[string]string, existing *gspb.Document, sessions map[string]gamesyncSession) error {
	collection, id, ok := documentPath(name)
	if !ok {
		return status.Error(codes.InvalidArgument, "invalid document path")
	}
	owner := lastResourceSegment(session.UserSession)
	denied := func() error { return status.Error(codes.PermissionDenied, "document is outside caller ownership") }
	switch collection {
	case "p", "e":
		if id != strconv.FormatInt(session.Sequence, 10) {
			return denied()
		}
		if uid := fields.GetFields()["uid"]; uid != nil && !observedStringValue(uid, session.UID) {
			return denied()
		}
	case "c":
		if !observedConnectionDocumentName(name) {
			return status.Error(codes.InvalidArgument, "invalid connection document")
		}
		if existing != nil && owners[name] != owner {
			return denied()
		}
	case "__pus", "__bl":
		if id != owner {
			return denied()
		}
		if fields != nil {
			key := "uid"
			if collection == "__bl" {
				key = "id"
			}
			if !observedStringValue(fields.GetFields()[key], session.UID) {
				return denied()
			}
			if collection == "__pus" && (!observedIntegerValue(fields.GetFields()["ussid"], session.Sequence) || !observedIntegerValue(fields.GetFields()["ucsid"], session.Connection)) {
				return denied()
			}
		}
	case "__stu", "__stg":
		// These are signalling envelopes, not server-generated station records.
		// Keys/types recovered at 0xD94910; never synthesize a payload or peer.
		if collection == "__stu" {
			recipient, ok := sessions[id]
			if !ok || recipient.GameSession != session.GameSession {
				return denied()
			}
		} else if id != "All" {
			return status.Error(codes.Unimplemented, "unknown signalling group")
		}
		if fields == nil {
			if owners[name] != owner {
				return denied()
			}
			return nil
		}
		f := fields.GetFields()
		if _, ok := f["suscid"].GetValueType().(*commonpb.Value_IntegerValue); !ok {
			return status.Error(codes.InvalidArgument, "signalling connection ID must be an integer")
		}
		if !observedStringValue(f["suid"], session.UID) || !observedStringValue(f["susid"], owner) || !observedIntegerValue(f["sussid"], session.Sequence) {
			return denied()
		}
		if _, ok := f["pl"].GetValueType().(*commonpb.Value_BytesValue); !ok {
			return status.Error(codes.InvalidArgument, "signalling payload must be bytes")
		}
		// Wonder's captured __stu envelope (2026-09-04, payload 000870)
		// omits mp. Validate it only when present; never invent metadata.
		if metadata, present := f["mp"]; present {
			if _, ok := metadata.GetValueType().(*commonpb.Value_MapValue); !ok {
				return status.Error(codes.InvalidArgument, "signalling metadata must be a map")
			}
		}
	default:
		return status.Error(codes.Unimplemented, "unsupported document family")
	}
	return nil
}

func applyFieldTransform(doc *gspb.Document, t *gspb.FieldTransform, globals map[string]*commonpb.Value, now *timestamppb.Timestamp) (*commonpb.Value, error) {
	path, err := fieldPath(t.FieldPath)
	if err != nil {
		return nil, err
	}
	current := readField(doc.Fields, path)
	integer := func(v *commonpb.Value) (int64, error) {
		if v == nil {
			return 0, nil
		}
		x, ok := v.GetValueType().(*commonpb.Value_IntegerValue)
		if !ok {
			return 0, status.Error(codes.InvalidArgument, "integer transform requires integer value")
		}
		return x.IntegerValue, nil
	}
	global := func(name string) error {
		if name != "__upcsidn" {
			return status.Error(codes.Unimplemented, "unknown transform global")
		}
		return nil
	}
	switch x := t.GetTransformType().(type) {
	case *gspb.FieldTransform_LoadGlobalValue:
		if err := global(x.LoadGlobalValue); err != nil {
			return nil, err
		}
		if v := globals[x.LoadGlobalValue]; v != nil {
			current = v
		} else if current == nil {
			current = gamesyncIntegerValue(0)
		}
	case *gspb.FieldTransform_StoreGlobalValue:
		if err := global(x.StoreGlobalValue); err != nil {
			return nil, err
		}
		if current == nil {
			return nil, status.Error(codes.FailedPrecondition, "cannot store a missing field")
		}
		globals[x.StoreGlobalValue] = proto.Clone(current).(*commonpb.Value)
	case *gspb.FieldTransform_ClearGlobalValue:
		if err := global(x.ClearGlobalValue); err != nil {
			return nil, err
		}
		delete(globals, x.ClearGlobalValue)
	case *gspb.FieldTransform_Increment:
		a, err := integer(current)
		if err != nil {
			return nil, err
		}
		b, err := integer(x.Increment)
		if err != nil {
			return nil, err
		}
		if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
			return nil, status.Error(codes.OutOfRange, "integer transform overflow")
		}
		current = gamesyncIntegerValue(a + b)
	case *gspb.FieldTransform_Maximum:
		a, err := integer(current)
		if err != nil {
			return nil, err
		}
		b, err := integer(x.Maximum)
		if err != nil {
			return nil, err
		}
		if b > a {
			a = b
		}
		current = gamesyncIntegerValue(a)
	case *gspb.FieldTransform_Minimum:
		a, err := integer(current)
		if err != nil {
			return nil, err
		}
		b, err := integer(x.Minimum)
		if err != nil {
			return nil, err
		}
		if b < a {
			a = b
		}
		current = gamesyncIntegerValue(a)
	case *gspb.FieldTransform_SetServerValue:
		if x.SetServerValue != gspb.FieldTransform_REQUEST_TIME {
			return nil, status.Error(codes.InvalidArgument, "unknown server transform")
		}
		current = &commonpb.Value{ValueType: &commonpb.Value_TimestampValue{TimestampValue: now}}
	default:
		return nil, status.Error(codes.Unimplemented, "unsupported field transform")
	}
	if current == nil {
		return nil, status.Error(codes.FailedPrecondition, "transform result missing")
	}
	setField(doc.Fields, path, current)
	return proto.Clone(current).(*commonpb.Value), nil
}

func (g *gamesyncServer) WriteDocuments(ctx context.Context, req *gspb.WriteDocumentsRequest) (*gspb.WriteDocumentsResponse, error) {
	session, err := g.authorizedSession(ctx, "")
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid session authorization")
	}
	if len(req.GetWriteOperations())+len(req.GetDefermentOperations()) > 256 {
		return nil, status.Error(codes.ResourceExhausted, "too many operations")
	}
	g.publication.Lock()
	defer g.publication.Unlock()
	g.mu.Lock()
	// Recheck after obtaining the commit lock: expiration must win over stale credentials.
	if _, ok := g.sessions[lastResourceSegment(session.UserSession)]; !ok {
		g.mu.Unlock()
		return nil, status.Error(codes.Unauthenticated, "session expired")
	}
	docs := map[string]*gspb.Document{}
	for name, d := range g.documents[session.GameSession] {
		docs[name] = proto.Clone(d).(*gspb.Document)
	}
	globals := map[string]*commonpb.Value{}
	for name, v := range g.globalValues[session.GameSession] {
		globals[name] = proto.Clone(v).(*commonpb.Value)
	}
	owners := map[string]string{}
	for name, v := range g.owners[session.GameSession] {
		owners[name] = v
	}
	owner := lastResourceSegment(session.UserSession)
	deferments := map[string]*gspb.Deferment{}
	for name, d := range g.deferments[owner] {
		deferments[name] = proto.Clone(d).(*gspb.Deferment)
	}
	changed := map[string]bool{}
	now := timestamppb.Now()
	results := []*gspb.WriteResult{}
	fail := func(err error) (*gspb.WriteDocumentsResponse, error) { g.mu.Unlock(); return nil, err }
	for _, op := range req.WriteOperations {
		var name string
		var fields *commonpb.MapValue
		var precondition *gspb.Precondition
		switch x := op.GetOperationType().(type) {
		case *gspb.WriteOperation_UpdateDocument:
			name = x.UpdateDocument.GetDocument().GetName()
			precondition = x.UpdateDocument.GetCurrentDocument()
		case *gspb.WriteOperation_MergeDocument:
			name = x.MergeDocument.GetDocument().GetName()
		case *gspb.WriteOperation_TransformDocument:
			name = x.TransformDocument.GetName()
		case *gspb.WriteOperation_DeleteDocument:
			name = x.DeleteDocument.GetName()
			precondition = x.DeleteDocument.GetCurrentDocument()
		default:
			return fail(status.Error(codes.InvalidArgument, "empty write operation"))
		}
		existing := docs[name]
		if err := checkPrecondition(precondition, existing); err != nil {
			return fail(err)
		}
		doc := &gspb.Document{Name: name, Fields: cloneFields(nil), CreateTime: now, UpdateTime: now}
		if existing != nil {
			doc = proto.Clone(existing).(*gspb.Document)
			doc.Fields = cloneFields(doc.Fields)
			doc.UpdateTime = now
		}
		var result *gspb.WriteResult
		switch x := op.GetOperationType().(type) {
		case *gspb.WriteOperation_UpdateDocument:
			if x.UpdateDocument.GetDocument().GetFields() == nil {
				return fail(status.Error(codes.InvalidArgument, "update fields required"))
			}
			mask := x.UpdateDocument.UpdateMask
			if mask == nil || len(mask.Paths) == 1 && mask.Paths[0] == "*" {
				doc.Fields = cloneFields(x.UpdateDocument.Document.Fields)
			} else {
				for _, p := range mask.Paths {
					path, err := fieldPath(p)
					if err != nil {
						return fail(err)
					}
					setField(doc.Fields, path, readField(x.UpdateDocument.Document.Fields, path))
				}
			}
			fields = doc.Fields
			result = &gspb.WriteResult{ResultType: &gspb.WriteResult_UpdateResult{UpdateResult: &gspb.UpdateResult{}}}
		case *gspb.WriteOperation_MergeDocument:
			if x.MergeDocument.GetDocument().GetFields() == nil {
				return fail(status.Error(codes.InvalidArgument, "merge fields required"))
			}
			for k, v := range x.MergeDocument.Document.Fields.Fields {
				if v != nil {
					doc.Fields.Fields[k] = proto.Clone(v).(*commonpb.Value)
				}
			}
			fields = doc.Fields
			result = &gspb.WriteResult{ResultType: &gspb.WriteResult_UpdateResult{UpdateResult: &gspb.UpdateResult{}}}
		case *gspb.WriteOperation_TransformDocument:
			if existing == nil {
				return fail(status.Error(codes.FailedPrecondition, "transform document missing"))
			}
			if !strings.HasPrefix(name, "docs/__pus/") {
				return fail(status.Error(codes.Unimplemented, "transforms currently limited to participant state"))
			}
			transformed := []*commonpb.Value{}
			for _, t := range x.TransformDocument.FieldTransforms {
				v, err := applyFieldTransform(doc, t, globals, now)
				if err != nil {
					return fail(err)
				}
				transformed = append(transformed, v)
			}
			fields = doc.Fields
			result = &gspb.WriteResult{ResultType: &gspb.WriteResult_TransformResult{TransformResult: &gspb.TransformResult{Results: transformed}}}
		case *gspb.WriteOperation_DeleteDocument:
			result = &gspb.WriteResult{ResultType: &gspb.WriteResult_DeleteResult{DeleteResult: &gspb.DeleteResult{}}}
		}
		if err := writableDocument(name, fields, session, owners, existing, g.sessions); err != nil {
			return fail(err)
		}
		if op.GetDeleteDocument() != nil {
			delete(docs, name)
			delete(owners, name)
		} else {
			docs[name] = doc
			owners[name] = owner
		}
		changed[name] = true
		results = append(results, result)
	}
	for _, op := range req.DefermentOperations {
		if deleted := op.GetDeleteDeferment(); deleted != nil {
			if !defermentBelongsToSession(deleted.Name, session) {
				return fail(status.Error(codes.PermissionDenied, "foreign deferment"))
			}
			delete(deferments, deleted.Name)
			continue
		}
		d := op.GetUpdateDeferment().GetDeferment()
		if d == nil || !defermentBelongsToSession(d.GetName(), session) || len(d.GetWriteOperations()) == 0 {
			return fail(status.Error(codes.InvalidArgument, "invalid cleanup deferment"))
		}
		for _, write := range d.WriteOperations {
			deleteOp := write.GetDeleteDocument()
			if deleteOp == nil {
				return fail(status.Error(codes.Unimplemented, "only cleanup-delete deferments supported"))
			}
			if err := writableDocument(deleteOp.Name, nil, session, owners, docs[deleteOp.Name], g.sessions); err != nil {
				return fail(err)
			}
		}
		deferments[d.Name] = proto.Clone(d).(*gspb.Deferment)
	}
	if g.globalValues == nil {
		g.globalValues = make(map[string]map[string]*commonpb.Value)
	}
	if g.owners == nil {
		g.owners = make(map[string]map[string]string)
	}
	if g.deferments == nil {
		g.deferments = make(map[string]map[string]*gspb.Deferment)
	}
	g.documents[session.GameSession] = docs
	g.globalValues[session.GameSession] = globals
	g.owners[session.GameSession] = owners
	g.deferments[owner] = deferments
	subscribers := []*gamesyncSubscriber{}
	for s := range g.subscribers[session.GameSession] {
		subscribers = append(subscribers, s)
	}
	g.mu.Unlock()
	names := []string{}
	for name := range changed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, s := range subscribers {
			if doc := docs[name]; doc != nil {
				_ = s.publish(proto.Clone(doc).(*gspb.Document))
			} else {
				_ = s.publishDeleted(name)
			}
		}
	}
	return &gspb.WriteDocumentsResponse{WriteResults: results, CommitTime: now}, nil
}

func (g *gamesyncServer) streamStarted(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.streamCounts == nil {
		g.streamCounts = map[string]int{}
	}
	g.streamCounts[id]++
	if timer := g.disconnectTimers[id]; timer != nil {
		timer.Stop()
		delete(g.disconnectTimers, id)
	}
}
func (g *gamesyncServer) streamEnded(id string) {
	g.mu.Lock()
	g.streamCounts[id]--
	last := g.streamCounts[id] <= 0
	g.mu.Unlock()
	if last {
		g.scheduleDisconnect(id, envDuration("NPLN_RECONNECT_GRACE", 30*time.Second))
	}
}
func (g *gamesyncServer) scheduleDisconnect(id string, delay time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.streamCounts[id] > 0 {
		return
	}
	if g.disconnectTimers == nil {
		g.disconnectTimers = map[string]*time.Timer{}
	}
	if timer := g.disconnectTimers[id]; timer != nil {
		timer.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() { g.expireSession(id, timer) })
	g.disconnectTimers[id] = timer
}
func (g *gamesyncServer) expireSession(id string, timer *time.Timer) {
	g.publication.Lock()
	defer g.publication.Unlock()
	g.mu.Lock()
	if g.streamCounts[id] > 0 || g.disconnectTimers[id] != timer {
		g.mu.Unlock()
		return
	}
	session, ok := g.sessions[id]
	if !ok {
		g.mu.Unlock()
		return
	}
	deleted := map[string]bool{"docs/__us/" + id: true}
	for _, deferment := range g.deferments[id] {
		for _, op := range deferment.WriteOperations {
			d := op.GetDeleteDocument()
			if d != nil && g.owners[session.GameSession][d.Name] == id && checkPrecondition(d.CurrentDocument, g.documents[session.GameSession][d.Name]) == nil {
				deleted[d.Name] = true
			}
		}
	}
	// Remove only this member's documents; never delete another sender's latest
	// message merely because it was addressed to a now-departing member.
	for name, owner := range g.owners[session.GameSession] {
		if owner == id {
			deleted[name] = true
		}
	}
	for name := range deleted {
		delete(g.documents[session.GameSession], name)
		delete(g.owners[session.GameSession], name)
	}
	delete(g.sessions, id)
	delete(g.deferments, id)
	delete(g.disconnectTimers, id)
	delete(g.streamCounts, id)
	subscribers := []*gamesyncSubscriber{}
	for s := range g.subscribers[session.GameSession] {
		subscribers = append(subscribers, s)
	}
	g.mu.Unlock()
	if g.registry != nil {
		g.registry.depart(session.GameSession, session.UserSession)
	}
	for name := range deleted {
		for _, s := range subscribers {
			_ = s.publishDeleted(name)
		}
	}
}
