package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	commonpb "npln.nintendo.net/npln-practice/proto/common"
	gspb "npln.nintendo.net/npln-practice/proto/gamesync/v1"
)

type fakeGamesyncStream struct {
	ctx       context.Context
	requests  []*gspb.KeepUserSessionRequest
	responses []*gspb.KeepUserSessionResponse
}

func (f *fakeGamesyncStream) Context() context.Context     { return f.ctx }
func (f *fakeGamesyncStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeGamesyncStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeGamesyncStream) SetTrailer(metadata.MD)       {}
func (f *fakeGamesyncStream) Send(response *gspb.KeepUserSessionResponse) error {
	f.responses = append(f.responses, response)
	return nil
}
func (f *fakeGamesyncStream) Recv() (*gspb.KeepUserSessionRequest, error) {
	if len(f.requests) == 0 {
		return nil, io.EOF
	}
	request := f.requests[0]
	f.requests = f.requests[1:]
	return request, nil
}
func (f *fakeGamesyncStream) SendMsg(message any) error {
	return f.Send(message.(*gspb.KeepUserSessionResponse))
}
func (f *fakeGamesyncStream) RecvMsg(message any) error {
	request, err := f.Recv()
	if err != nil {
		return err
	}
	proto.Merge(message.(*gspb.KeepUserSessionRequest), request)
	return nil
}

func TestGamesyncIssueTokenAcceptsServerMatchmakingToken(t *testing.T) {
	uid := "u-m7tjs2hidl4hhp2rlgfa"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	userSession := gameSession + "/userSessions/us-456"
	matchToken := mintGssMatchToken(uid, nplnTenant, gameSession, userSession, "Player", "{}", `{"latencies":{}}`)

	response, err := (&gamesyncServer{}).IssueToken(context.Background(), &gspb.IssueTokenRequest{
		UserSession:        userSession,
		MatchmakingIdToken: matchToken,
	})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	token := response.GetToken()
	if token.GetUserSession() != userSession || token.GetAccessToken() == "" || token.GetRefreshToken() == "" {
		t.Fatalf("incomplete token: %+v", token)
	}
	if token.GetTtl().AsDuration() != 8*time.Hour {
		t.Fatalf("ttl = %s", token.GetTtl().AsDuration())
	}
	parts := strings.Split(token.GetAccessToken(), ".")
	if len(parts) != 3 || !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		t.Fatal("Gamesync access token is not a valid server-signed JWT")
	}
}

func TestGamesyncIssueTokenAcceptsObservedShortSessionName(t *testing.T) {
	uid := "u-m7tjs2hidl4hhp2rlgfa"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	canonicalUserSession := gameSession + "/userSessions/us-456"
	shortUserSession := "userSessions/us-456"
	matchToken := mintGssMatchToken(uid, nplnTenant, gameSession, canonicalUserSession, "Player", "{}", `{"latencies":{}}`)

	response, err := (&gamesyncServer{}).IssueToken(context.Background(), &gspb.IssueTokenRequest{
		UserSession:        shortUserSession,
		MatchmakingIdToken: matchToken,
	})
	if err != nil {
		t.Fatalf("IssueToken with observed short name: %v", err)
	}
	if got := response.GetToken().GetUserSession(); got != shortUserSession {
		t.Fatalf("response user_session = %q, want observed alias %q", got, shortUserSession)
	}
}

func TestGamesyncIssueTokenRejectsMismatchedSession(t *testing.T) {
	uid := "u-m7tjs2hidl4hhp2rlgfa"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	userSession := gameSession + "/userSessions/us-456"
	matchToken := mintGssMatchToken(uid, nplnTenant, gameSession, userSession, "Player", "{}", `{"latencies":{}}`)

	_, err := (&gamesyncServer{}).IssueToken(context.Background(), &gspb.IssueTokenRequest{
		UserSession:        gameSession + "/userSessions/us-other",
		MatchmakingIdToken: matchToken,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("status = %s, want InvalidArgument", status.Code(err))
	}
}

func TestKeepUserSessionEchoAndEmptyTargetLifecycle(t *testing.T) {
	server := newGamesyncServer()
	session := "userSessions/us-456"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	server.rememberSession(gamesyncSession{
		UID:         "u-test",
		GameSession: gameSession,
		UserSession: session,
		Team:        "Player",
	})
	accessToken := mintSessionToken("u-test", nplnTenant, gameSession, session)
	stream := &fakeGamesyncStream{
		ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken)),
		requests: []*gspb.KeepUserSessionRequest{
			{Name: "userSessions/current", RequestType: &gspb.KeepUserSessionRequest_Echo{Echo: "heartbeat"}},
			{Name: "userSessions/current", RequestType: &gspb.KeepUserSessionRequest_UpdateTarget{UpdateTarget: &gspb.UpdateTargetRequest{
				Target: &gspb.Target{
					Name: "targets/target-1",
					TargetType: &gspb.Target_Documents{Documents: &gspb.DocumentsTarget{
						Documents: []string{"docs/__us/us-456"},
					}},
				},
			}}},
			{Name: "userSessions/current", RequestType: &gspb.KeepUserSessionRequest_DeleteTarget{DeleteTarget: &gspb.DeleteTargetRequest{Name: "targets/target-1"}}},
		},
	}
	if err := server.KeepUserSession(stream); err != nil {
		t.Fatalf("KeepUserSession: %v", err)
	}
	if len(stream.responses) != 5 {
		t.Fatalf("response count = %d, want 5", len(stream.responses))
	}
	if stream.responses[0].GetEcho() != "heartbeat" {
		t.Fatalf("echo = %q", stream.responses[0].GetEcho())
	}
	if got := stream.responses[1].GetTargetChange(); got.GetTargetId() != "target-1" || got.GetTargetChangeType() != gspb.TargetChange_UPDATED {
		t.Fatalf("first target change = %+v", got)
	}
	if got := stream.responses[2].GetDocumentChange(); got.GetDocumentChangeType() != gspb.DocumentChange_EXIST ||
		got.GetDocument().GetName() != "docs/__us/us-456" || got.GetDocument().GetFields().GetFields()["uid"].GetStringValue() != "u-test" ||
		got.GetDocument().GetFields().GetFields()["st"].GetIntegerValue() != 2 {
		t.Fatalf("document change = %+v", got)
	}
	if got := stream.responses[3].GetTargetChange(); got.GetTargetChangeType() != gspb.TargetChange_LISTED {
		t.Fatalf("second target change = %+v", got)
	}
	if got := stream.responses[4].GetTargetChange(); got.GetTargetChangeType() != gspb.TargetChange_DELETED {
		t.Fatalf("delete target change = %+v", got)
	}
}

func TestKeepUserSessionRejectsCurrentAliasWithoutBearerBinding(t *testing.T) {
	server := newGamesyncServer()
	server.rememberSession(gamesyncSession{
		UID:         "u-test",
		GameSession: nplnTenant + "/gameSessions/gs-123",
		UserSession: "userSessions/us-456",
	})
	stream := &fakeGamesyncStream{
		ctx: context.Background(),
		requests: []*gspb.KeepUserSessionRequest{
			{Name: "userSessions/current", RequestType: &gspb.KeepUserSessionRequest_Echo{Echo: "heartbeat"}},
		},
	}
	if err := server.KeepUserSession(stream); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status = %s, want Unauthenticated", status.Code(err))
	}
}

func TestWriteDocumentsStoresObservedCleanupDeferments(t *testing.T) {
	server := newGamesyncServer()
	userSession := "userSessions/us-456"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	server.rememberSession(gamesyncSession{
		UID:         "u-test",
		GameSession: gameSession,
		UserSession: userSession,
		Team:        "Player",
	})
	accessToken := mintSessionToken("u-test", nplnTenant, gameSession, userSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))
	deferment := func(kind string) *gspb.DefermentOperation {
		return &gspb.DefermentOperation{OperationType: &gspb.DefermentOperation_UpdateDeferment{
			UpdateDeferment: &gspb.UpdateDefermentRequest{Deferment: &gspb.Deferment{
				Name: "userSessions/current/deferments/" + kind + "/1",
				WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_DeleteDocument{
					DeleteDocument: &gspb.DeleteDocumentRequest{Name: "docs/" + kind + "/1"},
				}}},
			}},
		}}
	}

	response, err := server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{
		DefermentOperations: []*gspb.DefermentOperation{deferment("p"), deferment("e")},
	})
	if err != nil {
		t.Fatalf("WriteDocuments: %v", err)
	}
	if response.GetCommitTime() == nil || len(response.GetWriteResults()) != 0 {
		t.Fatalf("unexpected response: %+v", response)
	}
	server.mu.RLock()
	stored := len(server.deferments["us-456"])
	server.mu.RUnlock()
	if stored != 2 {
		t.Fatalf("stored deferments = %d, want 2", stored)
	}
}

func TestWriteDocumentsRejectsUnobservedDirectWrite(t *testing.T) {
	server := newGamesyncServer()
	userSession := "userSessions/us-456"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	server.rememberSession(gamesyncSession{UID: "u-test", GameSession: gameSession, UserSession: userSession})
	accessToken := mintSessionToken("u-test", nplnTenant, gameSession, userSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))

	_, err := server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{
		WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_DeleteDocument{
			DeleteDocument: &gspb.DeleteDocumentRequest{Name: "docs/unobserved/1"},
		}}},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("status = %s, want Unimplemented", status.Code(err))
	}
}

func TestWriteDocumentsStoresAndPublishesObservedPlayerDocument(t *testing.T) {
	server := newGamesyncServer()
	userSession := "userSessions/us-456"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	session := gamesyncSession{UID: "u-test", GameSession: gameSession, UserSession: userSession, Team: "Player"}
	server.rememberSession(session)
	accessToken := mintSessionToken("u-test", nplnTenant, gameSession, userSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))
	stream := &fakeGamesyncStream{ctx: ctx}
	subscriber := newGamesyncSubscriber(stream)
	target := &gspb.Target{
		Name: "userSessions/current/targets/4",
		TargetType: &gspb.Target_Collection{Collection: &gspb.CollectionTarget{
			Collection: "docs/p",
		}},
	}
	if err := subscriber.installTarget(target, nil); err != nil {
		t.Fatal(err)
	}
	server.addSubscriber(gameSession, subscriber)
	defer server.removeSubscriber(gameSession, subscriber)

	response, err := server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{
		WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_UpdateDocument{
			UpdateDocument: &gspb.UpdateDocumentRequest{
				Document: &gspb.Document{Name: "docs/p/1", Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"uid": gamesyncStringValue("u-test"),
				}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}},
			},
		}}},
	})
	if err != nil {
		t.Fatalf("WriteDocuments: %v", err)
	}
	if len(response.GetWriteResults()) != 1 || response.GetWriteResults()[0].GetUpdateResult() == nil {
		t.Fatalf("write results = %+v", response.GetWriteResults())
	}
	if len(stream.responses) != 3 {
		t.Fatalf("stream responses = %d, want snapshot pair plus live update", len(stream.responses))
	}
	change := stream.responses[2].GetDocumentChange()
	if change.GetTargetId() != "4" || change.GetDocumentChangeType() != gspb.DocumentChange_UPDATED ||
		change.GetDocument().GetName() != "docs/p/1" {
		t.Fatalf("published change = %+v", change)
	}
	server.mu.RLock()
	stored := server.documents[gameSession]["docs/p/1"]
	server.mu.RUnlock()
	if stored == nil || stored.GetCreateTime() == nil || stored.GetUpdateTime() == nil {
		t.Fatalf("stored document = %+v", stored)
	}
}

func TestWriteDocumentsStoresPrivateRoomConnectionDocumentAndCleanup(t *testing.T) {
	server := newGamesyncServer()
	userSession := "userSessions/us-456"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	session := gamesyncSession{UID: "u-test", GameSession: gameSession, UserSession: userSession, Team: "Player"}
	server.rememberSession(session)
	accessToken := mintSessionToken("u-test", nplnTenant, gameSession, userSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))
	stream := &fakeGamesyncStream{ctx: ctx}
	subscriber := newGamesyncSubscriber(stream)
	target := &gspb.Target{
		Name: "userSessions/current/targets/5",
		TargetType: &gspb.Target_Collection{Collection: &gspb.CollectionTarget{
			Collection: "docs/c",
		}},
	}
	if err := subscriber.installTarget(target, nil); err != nil {
		t.Fatal(err)
	}
	server.addSubscriber(gameSession, subscriber)
	defer server.removeSubscriber(gameSession, subscriber)

	const documentName = "docs/c/0000000000000002"
	document := &gspb.Document{Name: documentName, Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
		"cid":  {ValueType: &commonpb.Value_IntegerValue{IntegerValue: 2}},
		"pn":   gamesyncStringValue("3DWorldDev"),
		"uid":  gamesyncStringValue("us-456"),
		"usid": {ValueType: &commonpb.Value_IntegerValue{IntegerValue: 1}},
	}}}
	update := &gspb.WriteOperation{OperationType: &gspb.WriteOperation_UpdateDocument{
		UpdateDocument: &gspb.UpdateDocumentRequest{
			Document:        document,
			CurrentDocument: &gspb.Precondition{ConditionType: &gspb.Precondition_Exists{Exists: false}},
		},
	}}
	deferredDelete := &gspb.DefermentOperation{OperationType: &gspb.DefermentOperation_UpdateDeferment{
		UpdateDeferment: &gspb.UpdateDefermentRequest{Deferment: &gspb.Deferment{
			Name: "userSessions/current/deferments/c/0000000000000002",
			WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_DeleteDocument{
				DeleteDocument: &gspb.DeleteDocumentRequest{Name: documentName},
			}}},
		}},
	}}
	request := &gspb.WriteDocumentsRequest{
		WriteOperations:     []*gspb.WriteOperation{update},
		DefermentOperations: []*gspb.DefermentOperation{deferredDelete},
	}

	response, err := server.WriteDocuments(ctx, request)
	if err != nil {
		t.Fatalf("WriteDocuments: %v", err)
	}
	if len(response.GetWriteResults()) != 1 || response.GetWriteResults()[0].GetUpdateResult() == nil {
		t.Fatalf("write results = %+v", response.GetWriteResults())
	}
	server.mu.RLock()
	storedDocument := server.documents[gameSession][documentName]
	storedDeferment := server.deferments["us-456"]["userSessions/current/deferments/c/0000000000000002"]
	server.mu.RUnlock()
	if storedDocument == nil || storedDeferment == nil {
		t.Fatalf("private-room state was not stored: document=%+v deferment=%+v", storedDocument, storedDeferment)
	}
	if len(stream.responses) != 3 || stream.responses[2].GetDocumentChange().GetDocument().GetName() != documentName {
		t.Fatalf("connection document was not published: %+v", stream.responses)
	}
}

func TestWriteDocumentsRejectsMalformedPrivateRoomConnectionDocument(t *testing.T) {
	session := gamesyncSession{UID: "u-test", UserSession: "userSessions/us-456"}
	operation := &gspb.WriteOperation{OperationType: &gspb.WriteOperation_UpdateDocument{
		UpdateDocument: &gspb.UpdateDocumentRequest{
			Document:        &gspb.Document{Name: "docs/c/not-a-captured-id", Fields: &commonpb.MapValue{}},
			CurrentDocument: &gspb.Precondition{ConditionType: &gspb.Precondition_Exists{Exists: false}},
		},
	}}
	if writableDocument(operation.GetUpdateDocument().GetDocument().GetName(), operation.GetUpdateDocument().GetDocument().GetFields(), session, nil, nil, nil) == nil {
		t.Fatal("malformed docs/c path was accepted")
	}
}

func TestWriteDocumentsStoresObservedCourseBlocklistAndCleanup(t *testing.T) {
	server := newGamesyncServer()
	userSession := "userSessions/us-456"
	gameSession := nplnTenant + "/gameSessions/gs-123"
	session := gamesyncSession{UID: "u-test", GameSession: gameSession, UserSession: userSession, Team: "Player"}
	server.rememberSession(session)
	accessToken := mintSessionToken(session.UID, nplnTenant, gameSession, userSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))
	documentName := "docs/__bl/" + lastResourceSegment(userSession)
	defermentName := "userSessions/current/deferments/__bl/" + lastResourceSegment(userSession)

	response, err := server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{
		WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_UpdateDocument{
			UpdateDocument: &gspb.UpdateDocumentRequest{
				Document: &gspb.Document{Name: documentName, Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"bi": {ValueType: &commonpb.Value_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}},
					"id": gamesyncStringValue(session.UID),
				}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}},
			},
		}}},
		DefermentOperations: []*gspb.DefermentOperation{{OperationType: &gspb.DefermentOperation_UpdateDeferment{
			UpdateDeferment: &gspb.UpdateDefermentRequest{Deferment: &gspb.Deferment{
				Name: defermentName,
				WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_DeleteDocument{
					DeleteDocument: &gspb.DeleteDocumentRequest{Name: documentName},
				}}},
			}},
		}}},
	})
	if err != nil {
		t.Fatalf("WriteDocuments: %v", err)
	}
	if len(response.GetWriteResults()) != 1 || response.GetWriteResults()[0].GetUpdateResult() == nil {
		t.Fatalf("write results = %+v", response.GetWriteResults())
	}
	server.mu.RLock()
	storedDocument := server.documents[gameSession][documentName]
	storedDeferment := server.deferments["us-456"][defermentName]
	server.mu.RUnlock()
	if storedDocument == nil || storedDeferment == nil {
		t.Fatalf("course blocklist state was not stored: document=%+v deferment=%+v", storedDocument, storedDeferment)
	}
}

func TestWriteDocumentsRejectsCourseBlocklistOutsideAuthenticatedSession(t *testing.T) {
	session := gamesyncSession{UID: "u-test", UserSession: "userSessions/us-456"}
	operation := func(name, uid string) *gspb.WriteOperation {
		return &gspb.WriteOperation{OperationType: &gspb.WriteOperation_UpdateDocument{
			UpdateDocument: &gspb.UpdateDocumentRequest{
				Document: &gspb.Document{Name: name, Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"bi": {ValueType: &commonpb.Value_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}},
					"id": gamesyncStringValue(uid),
				}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}},
			},
		}}
	}
	for _, test := range []struct {
		name     string
		document string
		uid      string
	}{
		{name: "different session", document: "docs/__bl/us-other", uid: session.UID},
		{name: "different uid", document: "docs/__bl/us-456", uid: "u-other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if writableDocument(test.document, operation(test.document, test.uid).GetUpdateDocument().GetDocument().GetFields(), session, nil, nil, nil) == nil {
				t.Fatal("cross-session course blocklist write was accepted")
			}
		})
	}
}

func observedCourseParticipantStateRequest(session gamesyncSession) *gspb.WriteDocumentsRequest {
	if session.Sequence == 0 {
		session.Sequence = 1
	}
	if session.Connection == 0 {
		session.Connection = 1
	}
	userSessionID := lastResourceSegment(session.UserSession)
	documentName := "docs/__pus/" + userSessionID
	return &gspb.WriteDocumentsRequest{
		WriteOperations: []*gspb.WriteOperation{
			{OperationType: &gspb.WriteOperation_UpdateDocument{UpdateDocument: &gspb.UpdateDocumentRequest{
				Document: &gspb.Document{Name: documentName, Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"pgn":    gamesyncStringValue("All"),
					"ucsid":  gamesyncIntegerValue(session.Connection),
					"uid":    gamesyncStringValue(session.UID),
					"upcsid": gamesyncIntegerValue(0),
					"ussid":  gamesyncIntegerValue(session.Sequence),
				}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}},
			}}},
			{OperationType: &gspb.WriteOperation_TransformDocument{TransformDocument: &gspb.TransformDocumentRequest{
				Name: documentName,
				FieldTransforms: []*gspb.FieldTransform{
					{FieldPath: "`upcsid`", TransformType: &gspb.FieldTransform_LoadGlobalValue{LoadGlobalValue: "__upcsidn"}},
					{FieldPath: "`upcsid`", TransformType: &gspb.FieldTransform_Maximum{Maximum: gamesyncIntegerValue(20000)}},
					{FieldPath: "`upcsid`", TransformType: &gspb.FieldTransform_Increment{Increment: gamesyncIntegerValue(1)}},
					{FieldPath: "`upcsid`", TransformType: &gspb.FieldTransform_StoreGlobalValue{StoreGlobalValue: "__upcsidn"}},
				},
			}}},
		},
		DefermentOperations: []*gspb.DefermentOperation{{OperationType: &gspb.DefermentOperation_UpdateDeferment{
			UpdateDeferment: &gspb.UpdateDefermentRequest{Deferment: &gspb.Deferment{
				Name: "userSessions/current/deferments/__pus/" + userSessionID,
				WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_DeleteDocument{
					DeleteDocument: &gspb.DeleteDocumentRequest{Name: documentName},
				}}},
			}},
		}}},
	}
}

func gamesyncContextForSession(session gamesyncSession) context.Context {
	accessToken := mintSessionToken(session.UID, nplnTenant, session.GameSession, session.UserSession)
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))
}

func TestWriteDocumentsStoresTransformsAndPublishesObservedCourseParticipantState(t *testing.T) {
	server := newGamesyncServer()
	gameSession := nplnTenant + "/gameSessions/gs-shared"
	host := gamesyncSession{UID: "u-host", GameSession: gameSession, UserSession: "userSessions/us-host", Team: "Player"}
	guest := gamesyncSession{UID: "u-guest", GameSession: gameSession, UserSession: "userSessions/us-guest", Team: "Player"}
	server.rememberSession(host)
	server.rememberSession(guest)

	stream := &fakeGamesyncStream{ctx: gamesyncContextForSession(host)}
	subscriber := newGamesyncSubscriber(stream)
	target := &gspb.Target{
		Name: "userSessions/current/targets/6",
		TargetType: &gspb.Target_Collection{Collection: &gspb.CollectionTarget{
			Collection: "docs/__pus",
		}},
	}
	if err := subscriber.installTarget(target, server.targetSnapshot(host, target)); err != nil {
		t.Fatal(err)
	}
	server.addSubscriber(gameSession, subscriber)
	defer server.removeSubscriber(gameSession, subscriber)

	for index, session := range []gamesyncSession{host, guest} {
		session, _ = server.sessionByID(lastResourceSegment(session.UserSession))
		response, err := server.WriteDocuments(gamesyncContextForSession(session), observedCourseParticipantStateRequest(session))
		if err != nil {
			t.Fatalf("WriteDocuments(%s): %v", session.UID, err)
		}
		if len(response.GetWriteResults()) != 2 || response.GetWriteResults()[0].GetUpdateResult() == nil ||
			response.GetWriteResults()[1].GetTransformResult() == nil {
			t.Fatalf("write results(%s) = %+v", session.UID, response.GetWriteResults())
		}
		transformResults := response.GetWriteResults()[1].GetTransformResult().GetResults()
		wantCounter := int64(20001 + index)
		if len(transformResults) != 4 || transformResults[3].GetIntegerValue() != wantCounter {
			t.Fatalf("transform results(%s) = %+v, want final counter %d", session.UID, transformResults, wantCounter)
		}
	}

	server.mu.RLock()
	hostDocument := server.documents[gameSession]["docs/__pus/us-host"]
	guestDocument := server.documents[gameSession]["docs/__pus/us-guest"]
	globalCounter := server.globalValues[gameSession]["__upcsidn"]
	hostDeferment := server.deferments["us-host"]["userSessions/current/deferments/__pus/us-host"]
	guestDeferment := server.deferments["us-guest"]["userSessions/current/deferments/__pus/us-guest"]
	server.mu.RUnlock()
	if hostDocument.GetFields().GetFields()["upcsid"].GetIntegerValue() != 20001 ||
		guestDocument.GetFields().GetFields()["upcsid"].GetIntegerValue() != 20002 || globalCounter.GetIntegerValue() != 20002 {
		t.Fatalf("participant counters: host=%+v guest=%+v global=%+v", hostDocument, guestDocument, globalCounter)
	}
	if hostDeferment == nil || guestDeferment == nil {
		t.Fatalf("participant cleanup deferments were not stored: host=%+v guest=%+v", hostDeferment, guestDeferment)
	}

	// installTarget produces UPDATED and LISTED. Each compound update+transform
	// must then produce one final document notification rather than exposing the
	// intermediate upcsid=0 state.
	if len(stream.responses) != 4 {
		t.Fatalf("stream responses = %d, want target lifecycle plus two final updates", len(stream.responses))
	}
	for index, expected := range []struct {
		name   string
		upcsid int64
	}{
		{name: "docs/__pus/us-host", upcsid: 20001},
		{name: "docs/__pus/us-guest", upcsid: 20002},
	} {
		change := stream.responses[index+2].GetDocumentChange()
		if change.GetTargetId() != "6" || change.GetDocumentChangeType() != gspb.DocumentChange_UPDATED ||
			change.GetDocument().GetName() != expected.name ||
			change.GetDocument().GetFields().GetFields()["upcsid"].GetIntegerValue() != expected.upcsid {
			t.Fatalf("published participant change %d = %+v", index, change)
		}
	}

	snapshot := server.targetSnapshot(guest, target)
	if len(snapshot) != 2 || snapshot[0].GetName() != "docs/__pus/us-guest" || snapshot[1].GetName() != "docs/__pus/us-host" {
		t.Fatalf("participant snapshot = %+v", snapshot)
	}
}

func TestWriteDocumentsDeletesAndPublishesObservedCourseParticipantState(t *testing.T) {
	server := newGamesyncServer()
	session := gamesyncSession{
		UID: "u-test", GameSession: nplnTenant + "/gameSessions/gs-123", UserSession: "userSessions/us-456", Team: "Player",
	}
	server.rememberSession(session)
	ctx := gamesyncContextForSession(session)
	stream := &fakeGamesyncStream{ctx: ctx}
	subscriber := newGamesyncSubscriber(stream)
	target := &gspb.Target{
		Name: "userSessions/current/targets/6",
		TargetType: &gspb.Target_Collection{Collection: &gspb.CollectionTarget{
			Collection: "docs/__pus",
		}},
	}
	if err := subscriber.installTarget(target, nil); err != nil {
		t.Fatal(err)
	}
	server.addSubscriber(session.GameSession, subscriber)
	defer server.removeSubscriber(session.GameSession, subscriber)

	if _, err := server.WriteDocuments(ctx, observedCourseParticipantStateRequest(session)); err != nil {
		t.Fatalf("create participant state: %v", err)
	}
	response, err := server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{WriteOperations: []*gspb.WriteOperation{{
		OperationType: &gspb.WriteOperation_DeleteDocument{DeleteDocument: &gspb.DeleteDocumentRequest{Name: "docs/__pus/us-456"}},
	}}})
	if err != nil {
		t.Fatalf("delete participant state: %v", err)
	}
	if len(response.GetWriteResults()) != 1 || response.GetWriteResults()[0].GetDeleteResult() == nil {
		t.Fatalf("delete results = %+v", response.GetWriteResults())
	}
	server.mu.RLock()
	stored := server.documents[session.GameSession]["docs/__pus/us-456"]
	server.mu.RUnlock()
	if stored != nil {
		t.Fatalf("deleted participant document remains stored: %+v", stored)
	}
	if len(stream.responses) != 4 {
		t.Fatalf("stream responses = %d, want target lifecycle, update, and delete", len(stream.responses))
	}
	deleted := stream.responses[3].GetDocumentChange()
	if deleted.GetTargetId() != "6" || deleted.GetDocumentChangeType() != gspb.DocumentChange_DELETED ||
		deleted.GetDocument().GetName() != "docs/__pus/us-456" {
		t.Fatalf("published deletion = %+v", deleted)
	}
}

func TestWriteDocumentsRejectsMalformedCourseParticipantState(t *testing.T) {
	server := newGamesyncServer()
	session := gamesyncSession{
		UID: "u-test", GameSession: nplnTenant + "/gameSessions/gs-123", UserSession: "userSessions/us-456", Team: "Player",
	}
	server.rememberSession(session)
	ctx := gamesyncContextForSession(session)

	tests := []struct {
		name   string
		mutate func(*gspb.WriteDocumentsRequest)
	}{
		{name: "different uid", mutate: func(request *gspb.WriteDocumentsRequest) {
			request.GetWriteOperations()[0].GetUpdateDocument().GetDocument().GetFields().Fields["uid"] = gamesyncStringValue("u-other")
		}},
		{name: "different document session", mutate: func(request *gspb.WriteDocumentsRequest) {
			request.GetWriteOperations()[0].GetUpdateDocument().Document.Name = "docs/__pus/us-other"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := observedCourseParticipantStateRequest(session)
			test.mutate(request)
			_, err := server.WriteDocuments(ctx, request)
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("status = %v, want PermissionDenied", status.Code(err))
			}
		})
	}

	_, err := server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{WriteOperations: []*gspb.WriteOperation{{
		OperationType: &gspb.WriteOperation_TransformDocument{TransformDocument: observedCourseParticipantStateRequest(session).
			GetWriteOperations()[1].GetTransformDocument()},
	}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("transform without participant document status = %v, want FailedPrecondition", status.Code(err))
	}

	_, err = server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{WriteOperations: []*gspb.WriteOperation{{
		OperationType: &gspb.WriteOperation_DeleteDocument{DeleteDocument: &gspb.DeleteDocumentRequest{Name: "docs/__pus/us-other"}},
	}}})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-session delete status = %v, want PermissionDenied", status.Code(err))
	}
}

func storeObservedCourseBlocklist(t *testing.T, server *gamesyncServer, session gamesyncSession) {
	t.Helper()
	server.rememberSession(session)
	accessToken := mintSessionToken(session.UID, nplnTenant, session.GameSession, session.UserSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))
	documentName := "docs/__bl/" + lastResourceSegment(session.UserSession)
	_, err := server.WriteDocuments(ctx, &gspb.WriteDocumentsRequest{
		WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_UpdateDocument{
			UpdateDocument: &gspb.UpdateDocumentRequest{
				Document: &gspb.Document{Name: documentName, Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"bi": {ValueType: &commonpb.Value_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}},
					"id": gamesyncStringValue(session.UID),
				}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}},
			},
		}}},
	})
	if err != nil {
		t.Fatalf("WriteDocuments(%s): %v", session.UID, err)
	}
}

func TestListDocumentsReturnsObservedCourseBlocklistsFromAuthorizedSession(t *testing.T) {
	server := newGamesyncServer()
	gameSession := nplnTenant + "/gameSessions/gs-shared"
	host := gamesyncSession{UID: "u-host", GameSession: gameSession, UserSession: "userSessions/us-host", Team: "Player"}
	guest := gamesyncSession{UID: "u-guest", GameSession: gameSession, UserSession: "userSessions/us-guest", Team: "Player"}
	outsider := gamesyncSession{UID: "u-outsider", GameSession: nplnTenant + "/gameSessions/gs-other", UserSession: "userSessions/us-outsider", Team: "Player"}
	storeObservedCourseBlocklist(t, server, host)
	storeObservedCourseBlocklist(t, server, guest)
	storeObservedCourseBlocklist(t, server, outsider)

	accessToken := mintSessionToken(guest.UID, nplnTenant, guest.GameSession, guest.UserSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))
	response, err := server.ListDocuments(ctx, &gspb.ListDocumentsRequest{Parent: "docs/__bl", PageSize: 20})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if response.GetNextPageToken() != "" {
		t.Fatalf("unexpected next page token %q", response.GetNextPageToken())
	}
	if len(response.GetDocuments()) != 2 {
		t.Fatalf("documents = %+v", response.GetDocuments())
	}
	if response.GetDocuments()[0].GetName() != "docs/__bl/us-guest" ||
		response.GetDocuments()[1].GetName() != "docs/__bl/us-host" {
		t.Fatalf("documents are not session-scoped and sorted: %+v", response.GetDocuments())
	}
	for _, document := range response.GetDocuments() {
		if document.GetName() == "docs/__bl/us-outsider" {
			t.Fatal("cross-session document was returned")
		}
	}
}

func TestListDocumentsRequiresAuthorizedGamesyncSession(t *testing.T) {
	server := newGamesyncServer()
	_, err := server.ListDocuments(context.Background(), &gspb.ListDocumentsRequest{Parent: "docs/__bl", PageSize: 20})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status = %v, want Unauthenticated", status.Code(err))
	}
}

func TestListDocumentsValidatesPaginationAndSupportsMasks(t *testing.T) {
	server := newGamesyncServer()
	session := gamesyncSession{UID: "u-test", GameSession: nplnTenant + "/gameSessions/gs-123", UserSession: "userSessions/us-456", Team: "Player"}
	server.rememberSession(session)
	accessToken := mintSessionToken(session.UID, nplnTenant, session.GameSession, session.UserSession)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+accessToken))

	tests := []struct {
		name    string
		request *gspb.ListDocumentsRequest
		want    codes.Code
	}{
		{name: "missing request", want: codes.InvalidArgument},
		{name: "different parent", request: &gspb.ListDocumentsRequest{Parent: "docs/p", PageSize: 20}},
		{name: "different page size", request: &gspb.ListDocumentsRequest{Parent: "docs/__bl", PageSize: 19}},
		{name: "page token", request: &gspb.ListDocumentsRequest{Parent: "docs/__bl", PageSize: 20, PageToken: "next"}, want: codes.InvalidArgument},
		{name: "read mask", request: &gspb.ListDocumentsRequest{Parent: "docs/__bl", PageSize: 20, ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"id"}}}},
		{name: "show missing", request: &gspb.ListDocumentsRequest{Parent: "docs/__bl", PageSize: 20, ShowMissing: true}, want: codes.Unimplemented},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := server.ListDocuments(ctx, test.request)
			if status.Code(err) != test.want {
				t.Fatalf("status = %v, want %v", status.Code(err), test.want)
			}
		})
	}
}

func TestUserSessionCollectionSnapshotContainsCurrentParticipant(t *testing.T) {
	server := newGamesyncServer()
	session := gamesyncSession{
		UID:         "u-test",
		GameSession: nplnTenant + "/gameSessions/gs-123",
		UserSession: "userSessions/us-456",
		Team:        "Player",
	}
	server.rememberSession(session)
	target := &gspb.Target{
		Name: "userSessions/current/targets/2",
		TargetType: &gspb.Target_Collection{Collection: &gspb.CollectionTarget{
			Collection: "docs/__us",
		}},
	}
	documents := server.targetSnapshot(session, target)
	if len(documents) != 1 || documents[0].GetName() != "docs/__us/us-456" ||
		documents[0].GetFields().GetFields()["uid"].GetStringValue() != "u-test" {
		t.Fatalf("snapshot = %+v", documents)
	}
}

func TestUserSessionCollectionPublishesParticipantThatJoinsLater(t *testing.T) {
	server := newGamesyncServer()
	gameSession := nplnTenant + "/gameSessions/gs-shared"
	host := gamesyncSession{
		UID:         "u-host",
		GameSession: gameSession,
		UserSession: "userSessions/us-host",
		Team:        "Player",
	}
	server.rememberSession(host)

	stream := &fakeGamesyncStream{ctx: context.Background()}
	subscriber := newGamesyncSubscriber(stream)
	target := &gspb.Target{
		Name: "userSessions/current/targets/2",
		TargetType: &gspb.Target_Collection{Collection: &gspb.CollectionTarget{
			Collection: "docs/__us",
		}},
	}
	if err := subscriber.installTarget(target, server.targetSnapshot(host, target)); err != nil {
		t.Fatal(err)
	}
	server.addSubscriber(gameSession, subscriber)

	guest := gamesyncSession{
		UID:         "u-guest",
		GameSession: gameSession,
		UserSession: "userSessions/us-guest",
		Team:        "Player",
	}
	server.rememberSession(guest)

	var joined *gspb.DocumentChange
	for _, response := range stream.responses {
		change := response.GetDocumentChange()
		if change != nil && change.GetDocument().GetName() == "docs/__us/us-guest" {
			joined = change
		}
	}
	if joined == nil {
		t.Fatal("existing docs/__us subscriber was not notified about the guest")
	}
	if joined.GetTargetId() != "2" || joined.GetDocumentChangeType() != gspb.DocumentChange_UPDATED ||
		joined.GetDocument().GetFields().GetFields()["uid"].GetStringValue() != "u-guest" {
		t.Fatalf("unexpected joined-participant notification: %+v", joined)
	}
}
