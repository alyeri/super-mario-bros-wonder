package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	commonpb "npln.nintendo.net/npln-practice/proto/common"
	gspb "npln.nintendo.net/npln-practice/proto/gamesync/v1"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
)

func testRegistry() *sessionRegistry {
	r := newSessionRegistry()
	r.friendUIDs = func(_ context.Context, uid string) (map[string]bool, error) {
		return map[string]bool{uid: true, "u-host": true, "u-guest": true}, nil
	}
	return r
}
func testMatched(t *testing.T, m *matchmakerServer, uid, config, key string) *mmpb.MatchmakingTicket {
	t.Helper()
	ctx := callerContext(uid)
	ticket, err := m.CreateMatchmakingTicket(ctx, wonderPublicTicket(config, key))
	if err != nil {
		t.Fatal(err)
	}
	matched, ok := m.completeMatchmakingTicket(ctx, ticket.Name, []string{"current"})
	if !ok {
		t.Fatal("match failed")
	}
	return matched
}
func testGamesyncJoin(t *testing.T, g *gamesyncServer, matched *mmpb.MatchmakingTicket, uid string) gamesyncSession {
	t.Helper()
	for _, member := range matched.MatchedUserSessions {
		if userIDFromPath(member.GetUserDefinition().GetUser()) != uid {
			continue
		}
		_, err := g.IssueToken(context.Background(), &gspb.IssueTokenRequest{UserSession: member.UserSession, MatchmakingIdToken: member.MatchmakingIdToken})
		if err != nil {
			t.Fatal(err)
		}
		s, ok := g.sessionByID(lastResourceSegment(member.UserSession))
		if !ok {
			t.Fatal("missing gamesync member")
		}
		return s
	}
	t.Fatal("no caller member")
	return gamesyncSession{}
}
func playerWrite(s gamesyncSession, name string) *gspb.WriteDocumentsRequest {
	return &gspb.WriteDocumentsRequest{WriteOperations: []*gspb.WriteOperation{{OperationType: &gspb.WriteOperation_UpdateDocument{UpdateDocument: &gspb.UpdateDocumentRequest{Document: &gspb.Document{Name: fmt.Sprintf("docs/p/%d", s.Sequence), Fields: &commonpb.MapValue{Fields: map[string]*commonpb.Value{"uid": gamesyncStringValue(s.UID), "pn": gamesyncStringValue(name)}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}}}}}}}
}
func TestSharedRegistryTwoPlayersFixedDataAndCleanup(t *testing.T) {
	r := testRegistry()
	m := newMatchmaker(r)
	g := newGamesyncServer(r)
	service := newGameSessionServer(r)
	host := testGamesyncJoin(t, g, testMatched(t, m, "u-host", "CourseMatch_20221202", "same-level"), "u-host")
	guest := testGamesyncJoin(t, g, testMatched(t, m, "u-guest", "CourseMatch_20221202", "same-level"), "u-guest")
	if host.GameSession != guest.GameSession || host.Sequence == guest.Sequence || host.Connection == guest.Connection {
		t.Fatal("members not distinct in shared session")
	}
	for _, s := range []gamesyncSession{host, guest} {
		if _, err := g.WriteDocuments(gamesyncContextForSession(s), playerWrite(s, s.UID)); err != nil {
			t.Fatal(err)
		}
		if _, err := g.WriteDocuments(gamesyncContextForSession(s), observedCourseParticipantStateRequest(s)); err != nil {
			t.Fatal(err)
		}
	}
	fixedHost, err := g.GetDocument(gamesyncContextForSession(host), &gspb.GetDocumentRequest{Name: "docs/__gs/f"})
	if err != nil {
		t.Fatal(err)
	}
	fixedGuest, err := g.GetDocument(gamesyncContextForSession(guest), &gspb.GetDocumentRequest{Name: "docs/__gs/f"})
	if err != nil || !proto.Equal(fixedHost, fixedGuest) {
		t.Fatal("fixed data differs between peers")
	}
	if len(fixedHost.Fields.Fields) != 6 || len(fixedHost.Fields.Fields["rs"].GetBytesValue()) != 16 || fixedHost.Fields.Fields["maxu"].GetIntegerValue() != 4 {
		t.Fatal("fixed contract differs from local policy")
	}
	for _, s := range []gamesyncSession{host, guest} {
		name := fmt.Sprintf("docs/p/%d", s.Sequence)
		read, err := g.GetDocument(gamesyncContextForSession(host), &gspb.GetDocumentRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		target := &gspb.Target{TargetType: &gspb.Target_Documents{Documents: &gspb.DocumentsTarget{Documents: []string{name}}}}
		snapshot := g.targetSnapshot(guest, target)
		if len(snapshot) != 1 || !proto.Equal(read, snapshot[0]) {
			t.Fatal("read/snapshot mismatch")
		}
		us, err := g.GetDocument(gamesyncContextForSession(host), &gspb.GetDocumentRequest{Name: "docs/__us/" + lastResourceSegment(s.UserSession)})
		if err != nil {
			t.Fatal(err)
		}
		if us.Fields.Fields["att"].GetMapValue().GetFields()["MatchingKey"].GetStringValue() != "same-level" || us.Fields.Fields["ltc"].GetStringValue() == "" {
			t.Fatal("lost attributes/latency")
		}
	}
	bad := playerWrite(guest, "overwrite")
	bad.WriteOperations[0].GetUpdateDocument().Document.Name = fmt.Sprintf("docs/p/%d", host.Sequence)
	if _, err := g.WriteDocuments(gamesyncContextForSession(guest), bad); status.Code(err) != codes.PermissionDenied {
		t.Fatal("guest overwrote host")
	}
	g.streamStarted(lastResourceSegment(host.UserSession))
	g.streamEnded(lastResourceSegment(host.UserSession))
	g.streamStarted(lastResourceSegment(host.UserSession)) // resume cancels deferred cleanup
	g.mu.RLock()
	timer := g.disconnectTimers[lastResourceSegment(host.UserSession)]
	g.mu.RUnlock()
	if timer != nil {
		t.Fatal("resume did not cancel expiration")
	}
	before, err := service.GetGameSession(callerContext("u-host"), &mmpb.GetGameSessionRequest{Name: host.GameSession})
	if err != nil || before.CurrentParticipantCount != 2 {
		t.Fatal("registry membership mismatch")
	}
	g.streamEnded(lastResourceSegment(host.UserSession))
	g.mu.RLock()
	timer = g.disconnectTimers[lastResourceSegment(host.UserSession)]
	g.mu.RUnlock()
	timer.Stop()
	g.expireSession(lastResourceSegment(host.UserSession), timer)
	if _, ok := g.sessionByID(lastResourceSegment(host.UserSession)); ok {
		t.Fatal("departed member remains")
	}
	if _, err := g.GetDocument(gamesyncContextForSession(guest), &gspb.GetDocumentRequest{Name: fmt.Sprintf("docs/p/%d", host.Sequence)}); status.Code(err) != codes.NotFound {
		t.Fatal("host player state remains")
	}
	if _, err := g.GetDocument(gamesyncContextForSession(guest), &gspb.GetDocumentRequest{Name: fmt.Sprintf("docs/p/%d", guest.Sequence)}); err != nil {
		t.Fatal("guest state removed")
	}
	after, err := service.GetGameSession(callerContext("u-guest"), &mmpb.GetGameSessionRequest{Name: guest.GameSession})
	if err != nil || after.CurrentParticipantCount != 1 {
		t.Fatal("stale matchmaking count")
	}
}

func TestAtomicWritesPreconditionsAndMasks(t *testing.T) {
	g := newGamesyncServer()
	s := gamesyncSession{UID: "u-host", GameSession: "gs-test", UserSession: "userSessions/us-test"}
	g.rememberSession(s)
	s, _ = g.sessionByID("us-test")
	ctx := gamesyncContextForSession(s)
	write := playerWrite(s, "original")
	if _, err := g.WriteDocuments(ctx, write); err != nil {
		t.Fatal(err)
	}
	read, err := g.GetDocument(ctx, &gspb.GetDocumentRequest{Name: "docs/p/1"})
	if err != nil {
		t.Fatal(err)
	}
	failure := playerWrite(s, "should-not-commit")
	second := proto.Clone(failure.WriteOperations[0]).(*gspb.WriteOperation)
	second.GetUpdateDocument().CurrentDocument = &gspb.Precondition{ConditionType: &gspb.Precondition_Exists{Exists: false}}
	failure.WriteOperations = append(failure.WriteOperations, second)
	if _, err := g.WriteDocuments(ctx, failure); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("create-only precondition ignored")
	}
	still, err := g.GetDocument(ctx, &gspb.GetDocumentRequest{Name: "docs/p/1"})
	if err != nil || !proto.Equal(read, still) {
		t.Fatal("failed batch committed partial state")
	}
	patch := playerWrite(s, "updated")
	patch.WriteOperations[0].GetUpdateDocument().UpdateMask.Paths = []string{"`pn`"}
	if _, err := g.WriteDocuments(ctx, patch); err != nil {
		t.Fatal(err)
	}
	masked, err := g.GetDocument(ctx, &gspb.GetDocumentRequest{Name: "docs/p/1", ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"pn"}}})
	if err != nil || len(masked.Fields.Fields) != 1 || masked.Fields.Fields["pn"].GetStringValue() != "updated" {
		t.Fatal("read/write masks failed")
	}
	request := observedCourseParticipantStateRequest(s)
	request.WriteOperations[1].GetTransformDocument().FieldTransforms[1].TransformType = &gspb.FieldTransform_Maximum{Maximum: gamesyncIntegerValue(19999)}
	if _, err := g.WriteDocuments(ctx, request); err != nil {
		t.Fatal("integer transforms must not whitelist the captured constant")
	}
}

func TestFriendRoomsDiscoveryJoinPasswordAndPoolIsolation(t *testing.T) {
	r := testRegistry()
	svc := newGameSessionServer(r)
	mm := newMatchmaker(r)
	create := func(uid, password string) *mmpb.GameSessionCreationTicket {
		ctx := callerContext(uid)
		ticket, err := svc.CreateGameSessionCreationTicket(ctx, &mmpb.CreateGameSessionCreationTicketRequest{GameSessionCreationTicket: &mmpb.GameSessionCreationTicket{MatchmakingConfig: "matchmakingConfigs/FriendMatch", UserDefinitions: []*mmpb.UserDefinition{{User: "users/current"}}, GameSession: &mmpb.GameSession{Password: password}}})
		if err != nil {
			t.Fatal(err)
		}
		room, ok := svc.completeGameSessionCreationTicket(ctx, ticket.Name, []string{"current"})
		if !ok {
			t.Fatal("room not created")
		}
		again, ok := svc.completeGameSessionCreationTicket(ctx, ticket.Name, []string{"current"})
		if !ok || again.GameSession.Name != room.GameSession.Name {
			t.Fatal("ticket completion not idempotent")
		}
		return room
	}
	a := create("u-host", "1122")
	b := create("u-guest", "3344")
	rooms, err := svc.QueryGameSessions(callerContext("u-guest"), &mmpb.QueryGameSessionsRequest{Tenant: nplnTenant, Users: []string{"users/u-host"}, MinVacancyCount: 1})
	if err != nil || len(rooms.GameSessions) != 1 || rooms.GameSessions[0].Name != a.GameSession.Name || rooms.GameSessions[0].Password != "" {
		t.Fatalf("friend room lookup or password redaction failed: %v", err)
	}
	join := &mmpb.JoinGameSessionRequest{Name: a.GameSession.Name, Password: "wrong", UserDefinitions: []*mmpb.UserDefinition{{User: "users/current"}}, IncludeIdTokenUsers: []string{"current", "users/u-host"}}
	if _, err := svc.JoinGameSession(callerContext("u-guest"), join); status.Code(err) != codes.PermissionDenied {
		t.Fatal("wrong password accepted")
	}
	join.Password = "1122"
	joined, err := svc.JoinGameSession(callerContext("u-guest"), join)
	if err != nil || joined.GameSession.CurrentParticipantCount != 2 {
		t.Fatalf("join failed %v", err)
	}
	for _, u := range joined.MatchedUserSessions {
		if userIDFromPath(u.UserDefinition.User) == "u-host" && u.MatchmakingIdToken != "" {
			t.Fatal("leaked another member's bearer credential")
		}
	}
	pool := func(uid, room string) string {
		ctx := callerContext(uid)
		req := wonderPublicTicket("FriendWorldMapMatch_20230428", "same-map")
		req.MatchmakingTicket.UserDefinitions[0].Attributes.Fields["FriendGameSessionId"] = gamesyncStringValue(lastResourceSegment(room))
		ticket, err := mm.CreateMatchmakingTicket(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		matched, ok := mm.completeMatchmakingTicket(ctx, ticket.Name, []string{"current"})
		if !ok {
			t.Fatal("private pool failed")
		}
		return matched.GameSession.Name
	}
	if pool("u-host", a.GameSession.Name) == pool("u-guest", b.GameSession.Name) {
		t.Fatal("different friend rooms merged")
	}
	if got := pool("u-guest", a.GameSession.Name); got != pool("u-host", a.GameSession.Name) {
		t.Fatal("same-room members did not share map pool")
	}
	r.friendUIDs = func(_ context.Context, uid string) (map[string]bool, error) { return map[string]bool{uid: true}, nil }
	if _, err := svc.GetGameSession(callerContext("u-outsider"), &mmpb.GetGameSessionRequest{Name: a.GameSession.Name}); status.Code(err) != codes.NotFound {
		t.Fatal("private room visible to outsider")
	}
}

func TestMatchmakingIdentityCannotBeForgedWithMetadata(t *testing.T) {
	m := newMatchmaker()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("uid", "u-host"))
	if _, err := m.CreateMatchmakingTicket(ctx, wonderPublicTicket("CourseMatch", "key")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("unsigned routing hint accepted")
	}
	ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs("uid", "u-host", "authorization", "Bearer "+mintNplnAccessToken(1800000002, "u-guest", nplnTenant)))
	if _, err := m.CreateMatchmakingTicket(ctx, wonderPublicTicket("CourseMatch", "key")); status.Code(err) != codes.PermissionDenied {
		t.Fatal("mismatched routing hint accepted")
	}
}

// Keep the duration import useful for tests run with long background leases:
func TestGamesyncReconnectTimerDoesNotEvictResumedStream(t *testing.T) {
	g := newGamesyncServer()
	s := gamesyncSession{UID: "u-test", GameSession: "gs-test", UserSession: "userSessions/us-test"}
	g.rememberSession(s)
	g.scheduleDisconnect("us-test", time.Hour)
	g.mu.RLock()
	old := g.disconnectTimers["us-test"]
	g.mu.RUnlock()
	g.streamStarted("us-test")
	g.expireSession("us-test", old)
	if _, ok := g.sessionByID("us-test"); !ok {
		t.Fatal("old timer evicted resumed stream")
	}
}
