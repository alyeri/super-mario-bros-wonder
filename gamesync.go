package main

// Gamesync transports authenticated, session-scoped documents. The storage
// engine and bounded disconnect lifecycle live in gamesync_store.go.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	commonpb "npln.nintendo.net/npln-practice/proto/common"
	gspb "npln.nintendo.net/npln-practice/proto/gamesync/v1"
)

type gamesyncSession struct {
	UID         string
	GameSession string
	UserSession string
	Team        string
	Sequence    int64
	Connection  int64
	Attributes  *commonpb.MapValue
	LatencyJSON string
	CreatedAt   *timestamppb.Timestamp
}

type gamesyncSubscriber struct {
	mu      sync.Mutex
	stream  grpc.BidiStreamingServer[gspb.KeepUserSessionRequest, gspb.KeepUserSessionResponse]
	targets map[string]*gspb.Target
}

func newGamesyncSubscriber(stream grpc.BidiStreamingServer[gspb.KeepUserSessionRequest, gspb.KeepUserSessionResponse]) *gamesyncSubscriber {
	return &gamesyncSubscriber{
		stream:  stream,
		targets: make(map[string]*gspb.Target),
	}
}

func (s *gamesyncSubscriber) send(response *gspb.KeepUserSessionResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(response)
}

func targetMatchesDocument(target *gspb.Target, documentName string) bool {
	if documents := target.GetDocuments(); documents != nil {
		for _, name := range documents.GetDocuments() {
			if name == documentName {
				return true
			}
		}
	}
	if collection := target.GetCollection(); collection != nil {
		parent := documentName
		if separator := strings.LastIndexByte(parent, '/'); separator >= 0 {
			parent = parent[:separator]
		}
		return collection.GetCollection() == parent
	}
	return false
}

func (s *gamesyncSubscriber) installTarget(target *gspb.Target, documents []*gspb.Document) error {
	targetID := lastResourceSegment(target.GetName())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets[targetID] = proto.Clone(target).(*gspb.Target)
	if err := s.stream.Send(&gspb.KeepUserSessionResponse{
		ResponseType: &gspb.KeepUserSessionResponse_TargetChange{TargetChange: &gspb.TargetChange{
			TargetId: targetID, TargetChangeType: gspb.TargetChange_UPDATED,
		}},
	}); err != nil {
		return err
	}
	for _, document := range documents {
		if err := s.stream.Send(&gspb.KeepUserSessionResponse{
			ResponseType: &gspb.KeepUserSessionResponse_DocumentChange{DocumentChange: &gspb.DocumentChange{
				TargetId: targetID, DocumentChangeType: gspb.DocumentChange_EXIST, Document: document,
			}},
		}); err != nil {
			return err
		}
	}
	return s.stream.Send(&gspb.KeepUserSessionResponse{
		ResponseType: &gspb.KeepUserSessionResponse_TargetChange{TargetChange: &gspb.TargetChange{
			TargetId: targetID, TargetChangeType: gspb.TargetChange_LISTED,
		}},
	})
}

func (s *gamesyncSubscriber) deleteTarget(targetName string) error {
	targetID := lastResourceSegment(targetName)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.targets, targetID)
	return s.stream.Send(&gspb.KeepUserSessionResponse{
		ResponseType: &gspb.KeepUserSessionResponse_TargetChange{TargetChange: &gspb.TargetChange{
			TargetId: targetID, TargetChangeType: gspb.TargetChange_DELETED,
		}},
	})
}

func (s *gamesyncSubscriber) publish(document *gspb.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for targetID, target := range s.targets {
		if !targetMatchesDocument(target, document.GetName()) {
			continue
		}
		if err := s.stream.Send(&gspb.KeepUserSessionResponse{
			ResponseType: &gspb.KeepUserSessionResponse_DocumentChange{DocumentChange: &gspb.DocumentChange{
				TargetId: targetID, DocumentChangeType: gspb.DocumentChange_UPDATED, Document: document,
			}},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *gamesyncSubscriber) publishDeleted(documentName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for targetID, target := range s.targets {
		if !targetMatchesDocument(target, documentName) {
			continue
		}
		if err := s.stream.Send(&gspb.KeepUserSessionResponse{
			ResponseType: &gspb.KeepUserSessionResponse_DocumentChange{DocumentChange: &gspb.DocumentChange{
				TargetId: targetID, DocumentChangeType: gspb.DocumentChange_DELETED,
				Document: &gspb.Document{Name: documentName},
			}},
		}); err != nil {
			return err
		}
	}
	return nil
}

type gamesyncServer struct {
	gspb.UnimplementedGamesyncServer
	mu               sync.RWMutex
	registry         *sessionRegistry
	nextSequence     map[string]int64
	owners           map[string]map[string]string
	streamCounts     map[string]int
	disconnectTimers map[string]*time.Timer
	publication      sync.Mutex                                  // serialize commits and subscription snapshots
	sessions         map[string]gamesyncSession                  // concrete user-session id -> signed session binding
	deferments       map[string]map[string]*gspb.Deferment       // user-session id -> deferment name -> definition
	documents        map[string]map[string]*gspb.Document        // game-session name -> document name -> current value
	globalValues     map[string]map[string]*commonpb.Value       // game-session name -> transform global name -> current value
	subscribers      map[string]map[*gamesyncSubscriber]struct{} // game-session name -> active streams
}

func newGamesyncServer(registries ...*sessionRegistry) *gamesyncServer {
	server := &gamesyncServer{
		sessions:     make(map[string]gamesyncSession),
		deferments:   make(map[string]map[string]*gspb.Deferment),
		documents:    make(map[string]map[string]*gspb.Document),
		globalValues: make(map[string]map[string]*commonpb.Value),
		subscribers:  make(map[string]map[*gamesyncSubscriber]struct{}),
	}
	if len(registries) > 0 {
		server.registry = registries[0]
	}
	return server
}

func (g *gamesyncServer) addSubscriber(gameSession string, subscriber *gamesyncSubscriber) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.subscribers == nil {
		g.subscribers = make(map[string]map[*gamesyncSubscriber]struct{})
	}
	if g.subscribers[gameSession] == nil {
		g.subscribers[gameSession] = make(map[*gamesyncSubscriber]struct{})
	}
	g.subscribers[gameSession][subscriber] = struct{}{}
}

func (g *gamesyncServer) removeSubscriber(gameSession string, subscriber *gamesyncSubscriber) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.subscribers[gameSession], subscriber)
	if len(g.subscribers[gameSession]) == 0 {
		delete(g.subscribers, gameSession)
	}
}

func (g *gamesyncServer) rememberSession(session gamesyncSession) {
	g.publication.Lock()
	defer g.publication.Unlock()
	g.mu.Lock()
	if g.sessions == nil {
		g.sessions = make(map[string]gamesyncSession)
	}
	userSessionID := lastResourceSegment(session.UserSession)
	previous, existed := g.sessions[userSessionID]
	if existed {
		session.Sequence = previous.Sequence
		session.Connection = previous.Connection
		session.CreatedAt = previous.CreatedAt
	} else {
		if g.nextSequence == nil {
			g.nextSequence = make(map[string]int64)
		}
		g.nextSequence[session.GameSession]++
		session.Sequence = g.nextSequence[session.GameSession]
		session.Connection = session.Sequence
		session.CreatedAt = timestamppb.Now()
	}
	if session.Attributes == nil {
		session.Attributes = &commonpb.MapValue{Fields: map[string]*commonpb.Value{}}
	}
	if session.LatencyJSON == "" {
		session.LatencyJSON = `{"latencies":{}}`
	}
	g.sessions[userSessionID] = session
	if g.documents == nil {
		g.documents = make(map[string]map[string]*gspb.Document)
	}
	if g.documents[session.GameSession] == nil {
		g.documents[session.GameSession] = make(map[string]*gspb.Document)
	}
	userDocument, _ := observedUserSessionDocument("docs/__us/"+userSessionID, session)
	g.documents[session.GameSession][userDocument.Name] = userDocument
	subscribers := make([]*gamesyncSubscriber, 0, len(g.subscribers[session.GameSession]))
	for subscriber := range g.subscribers[session.GameSession] {
		subscribers = append(subscribers, subscriber)
	}
	g.mu.Unlock()

	// Existing collection subscribers must learn about participants that join a
	// pooled session after their initial docs/__us snapshot was listed.
	if existed {
		return
	}
	document, recognized := observedUserSessionDocument("docs/__us/"+userSessionID, session)
	if !recognized {
		return
	}
	for _, subscriber := range subscribers {
		if err := subscriber.publish(document); err != nil {
			log.Printf("[NPLN Gamesync] publish joined user_session=%q failed: %v", session.UserSession, err)
		}
	}
}

func (g *gamesyncServer) sessionByID(userSessionID string) (gamesyncSession, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	session, ok := g.sessions[userSessionID]
	return session, ok
}

type gssMatchClaims struct {
	Expires int64  `json:"exp"`
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	Game    struct {
		GameSessionID string `json:"gsid"`
		UserSessionID string `json:"usid"`
		TenantID      string `json:"tid"`
		UserID        string `json:"uid"`
		Team          string `json:"team"`
		Attributes    string `json:"attr"`
		Latency       string `json:"ltcy"`
	} `json:"gamesync"`
}

type gamesyncAccessClaims struct {
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	NPLN      struct {
		TenantID string `json:"tid"`
	} `json:"npln"`
	GSS struct {
		GameSession string `json:"game_session"`
		UserSession string `json:"user_session"`
	} `json:"gss"`
}

func verifyGssMatchToken(token string) (*gssMatchClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		return nil, fmt.Errorf("invalid ES256 signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var claims gssMatchClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	if claims.Issuer != "gss" {
		return nil, fmt.Errorf("issuer is %q", claims.Issuer)
	}
	if claims.Expires <= time.Now().Unix() {
		return nil, fmt.Errorf("match token expired")
	}
	if claims.Subject == "" || claims.Game.UserID != claims.Subject {
		return nil, fmt.Errorf("subject/user identity mismatch")
	}
	if claims.Game.TenantID != nplnTenantID || claims.Game.GameSessionID == "" || claims.Game.UserSessionID == "" {
		return nil, fmt.Errorf("invalid gamesync session binding")
	}
	return &claims, nil
}

func verifyGamesyncAccessToken(token string) (*gamesyncAccessClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		return nil, fmt.Errorf("invalid ES256 signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var claims gamesyncAccessClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	if claims.Issuer != nplnIssuer || claims.Subject == "" {
		return nil, fmt.Errorf("invalid issuer or subject")
	}
	if claims.ExpiresAt <= time.Now().Unix() {
		return nil, fmt.Errorf("token expired")
	}
	if claims.NPLN.TenantID != nplnTenantID || claims.GSS.GameSession == "" || claims.GSS.UserSession == "" {
		return nil, fmt.Errorf("invalid gamesync session binding")
	}
	return &claims, nil
}

func bearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("missing metadata")
	}
	for _, value := range md.Get("authorization") {
		value = strings.TrimSpace(value)
		if len(value) > len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
			return strings.TrimSpace(value[len("Bearer "):]), nil
		}
	}
	return "", fmt.Errorf("missing bearer token")
}

// authorizedSession resolves the observed userSessions/current alias only
// through the signed access token issued for this stream. It is deliberately
// not a global "current session" lookup.
func (g *gamesyncServer) authorizedSession(ctx context.Context, requestName string) (gamesyncSession, error) {
	token, err := bearerToken(ctx)
	if err != nil {
		return gamesyncSession{}, err
	}
	claims, err := verifyGamesyncAccessToken(token)
	if err != nil {
		return gamesyncSession{}, err
	}
	userSessionID := lastResourceSegment(claims.GSS.UserSession)
	session, ok := g.sessionByID(userSessionID)
	if !ok {
		return gamesyncSession{}, fmt.Errorf("token references unknown user session")
	}
	if session.UID != claims.Subject || session.GameSession != claims.GSS.GameSession ||
		lastResourceSegment(session.UserSession) != userSessionID {
		return gamesyncSession{}, fmt.Errorf("token does not match stored session")
	}
	if requestName != "" && requestName != "userSessions/current" && requestName != claims.GSS.UserSession &&
		lastResourceSegment(requestName) != userSessionID {
		return gamesyncSession{}, fmt.Errorf("request name does not match token session")
	}
	return session, nil
}

func defermentBelongsToSession(name string, session gamesyncSession) bool {
	return strings.HasPrefix(name, "userSessions/current/deferments/") ||
		strings.HasPrefix(name, strings.TrimSuffix(session.UserSession, "/")+"/deferments/")
}

func observedStringValue(value *commonpb.Value, expected string) bool {
	typed, ok := value.GetValueType().(*commonpb.Value_StringValue)
	return ok && typed.StringValue == expected
}

func observedIntegerValue(value *commonpb.Value, expected int64) bool {
	typed, ok := value.GetValueType().(*commonpb.Value_IntegerValue)
	return ok && typed.IntegerValue == expected
}

// Wonder creates one docs/c/<16-hex-digit-id> document while bringing a
// private room online. The captured operation is create-only (exists=false)
// and pairs it with a deferred delete for the same document.
func observedConnectionDocumentName(name string) bool {
	const prefix = "docs/c/"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	id := strings.TrimPrefix(name, prefix)
	if len(id) != 16 {
		return false
	}
	for _, char := range id {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

// Document writes are implemented atomically in gamesync_store.go.

func (g *gamesyncServer) IssueToken(ctx context.Context, req *gspb.IssueTokenRequest) (*gspb.IssueTokenResponse, error) {
	claims, err := verifyGssMatchToken(req.GetMatchmakingIdToken())
	if err != nil {
		log.Printf("[NPLN Gamesync] IssueToken rejected user_session=%q: %v", req.GetUserSession(), err)
		return nil, status.Error(codes.Unauthenticated, "invalid matchmaking identity token")
	}

	expectedUserSession := nplnTenant + "/gameSessions/" + claims.Game.GameSessionID + "/userSessions/" + claims.Game.UserSessionID
	shortUserSession := "userSessions/" + claims.Game.UserSessionID
	requestedUserSession := req.GetUserSession()
	if requestedUserSession != expectedUserSession && requestedUserSession != shortUserSession {
		log.Printf("[NPLN Gamesync] IssueToken rejected mismatched user_session=%q expected=%q or %q", requestedUserSession, expectedUserSession, shortUserSession)
		return nil, status.Error(codes.InvalidArgument, "user_session does not match matchmaking token")
	}

	gameSession := nplnTenant + "/gameSessions/" + claims.Game.GameSessionID
	var attributes *commonpb.MapValue
	latency := claims.Game.Latency
	if g.registry != nil {
		gs, member := g.registry.member(gameSession, requestedUserSession, claims.Subject)
		if member == nil {
			return nil, status.Error(codes.PermissionDenied, "match token references an inactive membership")
		}
		attributes = member.Attributes
		latency = gamesyncLatencyJSON(member.LatencyData)
		if err := g.initializeFixedData(gs); err != nil {
			return nil, status.Error(codes.Internal, "cannot initialize game session fixed data")
		}
	} else {
		var decodeErr error
		attributes, decodeErr = decodeGamesyncAttributes(claims.Game.Attributes)
		if decodeErr != nil {
			return nil, status.Error(codes.InvalidArgument, "malformed signed attributes")
		}
	}
	accessToken := mintSessionToken(claims.Subject, nplnTenant, gameSession, requestedUserSession)
	g.rememberSession(gamesyncSession{
		UID:         claims.Subject,
		GameSession: gameSession,
		UserSession: requestedUserSession,
		Team:        claims.Game.Team,
		Attributes:  attributes, LatencyJSON: latency,
	})
	g.scheduleDisconnect(lastResourceSegment(requestedUserSession), 2*time.Minute)
	log.Printf("[NPLN Gamesync] IssueToken success user=%q game_session=%q user_session=%q",
		claims.Subject, gameSession, requestedUserSession)
	return &gspb.IssueTokenResponse{Token: &gspb.Token{
		UserSession:  requestedUserSession,
		AccessToken:  accessToken,
		RefreshToken: accessToken,
		Ttl:          durationpb.New(nplnTokenTTL),
	}}, nil
}

func gamesyncStringValue(value string) *commonpb.Value {
	return &commonpb.Value{ValueType: &commonpb.Value_StringValue{StringValue: value}}
}

func gamesyncIntegerValue(value int64) *commonpb.Value {
	return &commonpb.Value{ValueType: &commonpb.Value_IntegerValue{IntegerValue: value}}
}

func gamesyncMapValue(value *commonpb.MapValue) *commonpb.Value {
	return &commonpb.Value{ValueType: &commonpb.Value_MapValue{MapValue: value}}
}

// User-session fields/types are read by Wonder's parser at 0x1CA268C.
// In particular ltc is a serialized string, att is a typed map, and the two
// integer identifiers distinguish active participants inside a GameSession.
func observedUserSessionDocument(name string, session gamesyncSession) (*gspb.Document, bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	userSessionID := lastResourceSegment(session.UserSession)
	if len(parts) != 3 || parts[0] != "docs" || parts[1] != "__us" || parts[2] != userSessionID {
		return nil, false
	}
	now := session.CreatedAt
	if now == nil {
		now = timestamppb.Now()
	}
	return &gspb.Document{
		Name: name,
		Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
			"uid":   gamesyncStringValue(session.UID),
			"ussid": gamesyncIntegerValue(session.Sequence),
			"ucsid": gamesyncIntegerValue(session.Connection),
			"st":    gamesyncIntegerValue(2),
			"tn":    gamesyncStringValue(session.Team),
			"att":   gamesyncMapValue(session.Attributes),
			"ltc":   gamesyncStringValue(session.LatencyJSON),
		}},
		CreateTime: now,
		UpdateTime: now,
	}, true
}

func (g *gamesyncServer) targetSnapshot(session gamesyncSession, target *gspb.Target) []*gspb.Document {
	return g.committedSnapshot(session, target)
}

// KeepUserSession implements the stream operations now demonstrated by Wonder:
// signed-session validation, echo heartbeats, and target lifecycle responses.
func (g *gamesyncServer) KeepUserSession(stream grpc.BidiStreamingServer[gspb.KeepUserSessionRequest, gspb.KeepUserSessionResponse]) error {
	session, authErr := g.authorizedSession(stream.Context(), "")
	if authErr != nil {
		log.Printf("[NPLN Gamesync] KeepUserSession rejected at open: %v", authErr)
		return status.Error(codes.Unauthenticated, "invalid user session authorization")
	}
	subscriber := newGamesyncSubscriber(stream)
	g.addSubscriber(session.GameSession, subscriber)
	defer g.removeSubscriber(session.GameSession, subscriber)
	g.streamStarted(lastResourceSegment(session.UserSession))
	defer g.streamEnded(lastResourceSegment(session.UserSession))
	log.Printf("[NPLN Gamesync] KeepUserSession stream opened user_session=%q", session.UserSession)
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			log.Printf("[NPLN Gamesync] KeepUserSession closed by client")
			return nil
		}
		if err != nil {
			log.Printf("[NPLN Gamesync] KeepUserSession receive error: %v", err)
			return err
		}
		requestSession, authErr := g.authorizedSession(stream.Context(), req.GetName())
		if authErr != nil {
			log.Printf("[NPLN Gamesync] KeepUserSession rejected session=%q: %v", req.GetName(), authErr)
			return status.Error(codes.Unauthenticated, "invalid user session authorization")
		}
		session = requestSession

		if echo := req.GetEcho(); echo != "" {
			if err := subscriber.send(&gspb.KeepUserSessionResponse{
				ResponseType: &gspb.KeepUserSessionResponse_Echo{Echo: echo},
			}); err != nil {
				return err
			}
			continue
		}

		if update := req.GetUpdateTarget(); update != nil && update.GetTarget() != nil {
			target := update.GetTarget()
			documentCount := 0
			collection := ""
			if documents := target.GetDocuments(); documents != nil {
				documentCount = len(documents.GetDocuments())
			}
			if collectionTarget := target.GetCollection(); collectionTarget != nil {
				collection = collectionTarget.GetCollection()
			}
			log.Printf("[NPLN Gamesync] KeepUserSession update_target=%q documents=%d collection=%q",
				target.GetName(), documentCount, collection)
			g.publication.Lock()
			snapshot := g.targetSnapshot(session, target)
			log.Printf("[NPLN Gamesync] KeepUserSession target=%q initial_documents=%d", target.GetName(), len(snapshot))
			installErr := subscriber.installTarget(target, snapshot)
			g.publication.Unlock()
			if installErr != nil {
				return installErr
			}
			continue
		}

		if deleted := req.GetDeleteTarget(); deleted != nil {
			log.Printf("[NPLN Gamesync] KeepUserSession delete_target=%q", deleted.GetName())
			if err := subscriber.deleteTarget(deleted.GetName()); err != nil {
				return err
			}
		}
	}
}
