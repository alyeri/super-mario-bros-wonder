package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	commonpb "npln.nintendo.net/npln-practice/proto/common"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
)

func wonderCallerContext() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"npln-tenant-id", nplnTenantID,
		"uid", "u-1800000001",
		"authorization", "Bearer "+mintNplnAccessToken(1800000001, "u-1800000001", nplnTenant),
	))
}

func TestMatchmakingPreservesCapturedDefinitionAndResolvesAliases(t *testing.T) {
	mm := newMatchmaker()
	ctx := wonderCallerContext()
	request := &mmpb.CreateMatchmakingTicketRequest{
		Parent: "tenants/current",
		MatchmakingTicket: &mmpb.MatchmakingTicket{
			MatchmakingConfig: "tenants/current/matchmakingConfigs/WorldMapMatch_20221202",
			UserDefinitions: []*mmpb.UserDefinition{{
				User: "tenants/current/users/current",
				Team: "Player",
				Attributes: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"GameSessionKey": {
						ValueType: &commonpb.Value_IntegerValue{IntegerValue: 1162109555},
					},
					"MatchingKey": {
						ValueType: &commonpb.Value_StringValue{StringValue: "0.1.3.no_rev_info.1"},
					},
				}},
			}},
		},
	}

	created, err := mm.CreateMatchmakingTicket(ctx, request)
	if err != nil {
		t.Fatalf("CreateMatchmakingTicket: %v", err)
	}
	if !strings.HasPrefix(created.GetName(), nplnTenant+"/matchmakingTickets/") {
		t.Fatalf("ticket name did not resolve tenant alias: %q", created.GetName())
	}
	if got := created.GetMatchmakingConfig(); got != nplnTenant+"/matchmakingConfigs/WorldMapMatch_20221202" {
		t.Fatalf("config = %q", got)
	}
	if got := created.GetUserDefinitions()[0].GetUser(); got != nplnTenant+"/users/u-1800000001" {
		t.Fatalf("user = %q", got)
	}
	if request.GetMatchmakingTicket().GetUserDefinitions()[0].GetUser() != "tenants/current/users/current" {
		t.Fatal("CreateMatchmakingTicket mutated the request")
	}

	aliasName := "tenants/current/matchmakingTickets/" + lastResourceSegment(created.GetName())
	completed, ok := mm.completeMatchmakingTicket(ctx, aliasName, []string{"tenants/current/users/current"})
	if !ok {
		t.Fatal("ticket lookup by tenants/current alias failed")
	}
	if completed.GetName() != created.GetName() {
		t.Fatalf("completed ticket name = %q, want concrete %q", completed.GetName(), created.GetName())
	}
	if completed.GetState() != mmpb.MatchmakingTicket_SUCCEEDED {
		t.Fatalf("state = %s", completed.GetState())
	}
	if len(completed.GetMatchedUserSessions()) != len(completed.GetUserDefinitions()) {
		t.Fatalf("matched/user definition counts differ: %d/%d", len(completed.GetMatchedUserSessions()), len(completed.GetUserDefinitions()))
	}
	matched := completed.GetMatchedUserSessions()[0]
	if matched.GetUserDefinition().GetUser() != nplnTenant+"/users/u-1800000001" {
		t.Fatalf("matched user definition missing or wrong: %+v", matched.GetUserDefinition())
	}
	if matched.GetMatchmakingIdToken() == "" {
		t.Fatal("missing matchmaking token")
	}
	gs := completed.GetGameSession()
	if gs == nil || gs.GetMaxParticipantCount() != 12 || gs.GetCurrentParticipantCount() != 1 || !gs.GetCanParticipate() || !gs.GetIsPublic() || gs.GetCreateTime() == nil {
		t.Fatalf("incomplete game session: %+v", gs)
	}
}

func callerContext(uid string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"npln-tenant-id", nplnTenantID,
		"uid", uid,
		"authorization", "Bearer "+mintNplnAccessToken(1800000001, uid, nplnTenant),
	))
}

func wonderPublicTicket(config, matchingKey string) *mmpb.CreateMatchmakingTicketRequest {
	return &mmpb.CreateMatchmakingTicketRequest{
		Parent: "tenants/current",
		MatchmakingTicket: &mmpb.MatchmakingTicket{
			MatchmakingConfig: "tenants/current/matchmakingConfigs/" + config,
			UserDefinitions: []*mmpb.UserDefinition{{
				User: "tenants/current/users/current",
				Team: "Player",
				Attributes: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"MatchingKey": {ValueType: &commonpb.Value_StringValue{StringValue: matchingKey}},
				}},
			}},
		},
	}
}

func TestCompatiblePublicTicketsShareOneGameSession(t *testing.T) {
	mm := newMatchmaker()
	hostCtx := callerContext("u-host")
	guestCtx := callerContext("u-guest")

	hostTicket, err := mm.CreateMatchmakingTicket(hostCtx, wonderPublicTicket("CourseMatch_20221202", "same-course"))
	if err != nil {
		t.Fatal(err)
	}
	hostResult, ok := mm.completeMatchmakingTicket(hostCtx, hostTicket.GetName(), []string{"tenants/current/users/current"})
	if !ok {
		t.Fatal("host ticket did not complete")
	}

	guestTicket, err := mm.CreateMatchmakingTicket(guestCtx, wonderPublicTicket("CourseMatch_20221202", "same-course"))
	if err != nil {
		t.Fatal(err)
	}
	guestResult, ok := mm.completeMatchmakingTicket(guestCtx, guestTicket.GetName(), []string{"tenants/current/users/current"})
	if !ok {
		t.Fatal("guest ticket did not complete")
	}
	if hostResult.GetGameSession().GetName() != guestResult.GetGameSession().GetName() {
		t.Fatalf("compatible tickets got different sessions: %q / %q", hostResult.GetGameSession().GetName(), guestResult.GetGameSession().GetName())
	}
	if got := guestResult.GetGameSession().GetCurrentParticipantCount(); got != 2 {
		t.Fatalf("participant count = %d, want 2", got)
	}
	if got := guestResult.GetGameSession().GetMaxParticipantCount(); got != 4 {
		t.Fatalf("course capacity = %d, want 4", got)
	}
	if got := len(guestResult.GetMatchedUserSessions()); got != 2 {
		t.Fatalf("matched users = %d, want 2", got)
	}

	for _, matched := range guestResult.GetMatchedUserSessions() {
		uid := userIDFromPath(matched.GetUserDefinition().GetUser())
		if uid == "u-guest" {
			if matched.GetMatchmakingIdToken() == "" {
				t.Fatal("guest did not receive its own matchmaking token")
			}
			claims, err := verifyGssMatchToken(matched.GetMatchmakingIdToken())
			if err != nil {
				t.Fatalf("guest token did not verify: %v", err)
			}
			if claims.Game.GameSessionID != lastResourceSegment(guestResult.GetGameSession().GetName()) || claims.Game.UserID != "u-guest" {
				t.Fatalf("guest token points at wrong identity/session: %+v", claims.Game)
			}
		} else if matched.GetMatchmakingIdToken() != "" {
			t.Fatalf("guest response leaked another user's token for %q", uid)
		}
	}

	refreshedHost, ok := mm.completeMatchmakingTicket(hostCtx, hostTicket.GetName(), []string{"tenants/current/users/current"})
	if !ok || refreshedHost.GetGameSession().GetCurrentParticipantCount() != 2 || len(refreshedHost.GetMatchedUserSessions()) != 2 {
		t.Fatalf("host did not observe shared placement on refresh: %+v", refreshedHost)
	}
}

func TestIncompatiblePublicTicketsUseDifferentGameSessions(t *testing.T) {
	mm := newMatchmaker()
	oneCtx := callerContext("u-one")
	twoCtx := callerContext("u-two")
	one, _ := mm.CreateMatchmakingTicket(oneCtx, wonderPublicTicket("CourseMatch_20221202", "course-a"))
	two, _ := mm.CreateMatchmakingTicket(twoCtx, wonderPublicTicket("CourseMatch_20221202", "course-b"))
	oneResult, _ := mm.completeMatchmakingTicket(oneCtx, one.GetName(), []string{"tenants/current/users/current"})
	twoResult, _ := mm.completeMatchmakingTicket(twoCtx, two.GetName(), []string{"tenants/current/users/current"})
	if oneResult.GetGameSession().GetName() == twoResult.GetGameSession().GetName() {
		t.Fatal("different MatchingKey values were placed in the same game session")
	}
}

func TestPrivateRoomCreationPreservesWonderRequestAndMintsGamesyncToken(t *testing.T) {
	gs := newGameSessionServer()
	ctx := wonderCallerContext()
	request := &mmpb.CreateGameSessionCreationTicketRequest{
		Parent: "tenants/current",
		GameSessionCreationTicket: &mmpb.GameSessionCreationTicket{
			MatchmakingConfig: "tenants/current/matchmakingConfigs/FriendMatch",
			UserDefinitions: []*mmpb.UserDefinition{{
				User: "tenants/current/users/current",
				Attributes: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"SharePlayLeaderUserId": {
						ValueType: &commonpb.Value_StringValue{StringValue: ""},
					},
				}},
				LatencyData: &mmpb.LatencyData{},
				Team:        "Player",
			}},
			GameSession: &mmpb.GameSession{
				Password: "1122",
				Properties: &commonpb.MapValue{Fields: map[string]*commonpb.Value{
					"MatchingKey": {
						ValueType: &commonpb.Value_StringValue{StringValue: "3.no_rev_info"},
					},
				}},
			},
		},
	}

	created, err := gs.CreateGameSessionCreationTicket(ctx, request)
	if err != nil {
		t.Fatalf("CreateGameSessionCreationTicket: %v", err)
	}
	if !strings.HasPrefix(created.GetName(), nplnTenant+"/gameSessionCreationTickets/") {
		t.Fatalf("ticket name did not resolve tenant alias: %q", created.GetName())
	}
	if created.GetState() != mmpb.GameSessionCreationTicket_PENDING {
		t.Fatalf("created state = %s", created.GetState())
	}
	if got := created.GetMatchmakingConfig(); got != nplnTenant+"/matchmakingConfigs/FriendMatch" {
		t.Fatalf("config = %q", got)
	}
	if got := created.GetUserDefinitions()[0].GetUser(); got != nplnTenant+"/users/u-1800000001" {
		t.Fatalf("user = %q", got)
	}
	if got := created.GetGameSession().GetPassword(); got != "1122" {
		t.Fatalf("password = %q", got)
	}
	if request.GetGameSessionCreationTicket().GetUserDefinitions()[0].GetUser() != "tenants/current/users/current" {
		t.Fatal("CreateGameSessionCreationTicket mutated the request")
	}

	aliasName := "tenants/current/gameSessionCreationTickets/" + lastResourceSegment(created.GetName())
	completed, ok := gs.completeGameSessionCreationTicket(ctx, aliasName, []string{"tenants/current/users/current"})
	if !ok {
		t.Fatal("creation-ticket lookup by tenants/current alias failed")
	}
	if completed.GetName() != created.GetName() {
		t.Fatalf("completed ticket name = %q, want concrete %q", completed.GetName(), created.GetName())
	}
	if completed.GetState() != mmpb.GameSessionCreationTicket_SUCCEEDED {
		t.Fatalf("completed state = %s", completed.GetState())
	}
	if len(completed.GetMatchedUserSessions()) != 1 {
		t.Fatalf("matched sessions = %d", len(completed.GetMatchedUserSessions()))
	}
	matched := completed.GetMatchedUserSessions()[0]
	if got := matched.GetUserDefinition().GetUser(); got != nplnTenant+"/users/u-1800000001" {
		t.Fatalf("matched user = %q", got)
	}
	claims, err := verifyGssMatchToken(matched.GetMatchmakingIdToken())
	if err != nil {
		t.Fatalf("private-room matchmaking token rejected by Gamesync verifier: %v", err)
	}
	if claims.Game.Team != "Player" || claims.Game.UserID != "u-1800000001" {
		t.Fatalf("wrong gamesync identity claims: %+v", claims.Game)
	}

	room := completed.GetGameSession()
	if room == nil || room.GetName() == "" || room.GetState() != mmpb.GameSession_ACTIVE {
		t.Fatalf("missing active game session: %+v", room)
	}
	if room.GetPassword() != "1122" || room.GetIsPublic() {
		t.Fatalf("private-room flags not preserved: password=%q public=%t", room.GetPassword(), room.GetIsPublic())
	}
	if room.GetMaxParticipantCount() != 12 || room.GetCurrentParticipantCount() != 1 || !room.GetCanParticipate() || room.GetCreateTime() == nil {
		t.Fatalf("incomplete game session: %+v", room)
	}
	if len(room.GetUserSessions()) != 1 || room.GetUserSessions()[0].GetName() != matched.GetUserSession() {
		t.Fatalf("game/matched user sessions disagree: %+v / %+v", room.GetUserSessions(), matched)
	}
	properties := room.GetProperties().GetFields()
	if got := properties["MatchingKey"].GetStringValue(); got != "3.no_rev_info" {
		t.Fatalf("MatchingKey = %q", got)
	}
	if got := properties["_BaseConfigName"].GetStringValue(); got != "FriendMatch" {
		t.Fatalf("_BaseConfigName = %q", got)
	}
	if _, ok := gs.sessions[lastResourceSegment(room.GetName())]; !ok {
		t.Fatal("completed room was not persisted")
	}
}

func TestMintGssMatchTokenClaims(t *testing.T) {
	token := mintGssMatchToken(
		"u-1800000001",
		nplnTenant,
		nplnTenant+"/gameSessions/gs-123",
		nplnTenant+"/gameSessions/gs-123/userSessions/us-456",
		"Player",
		`{"MatchingKey":{"type":"string","value":"0.1.3.no_rev_info.1"}}`,
		`{"latencies":{}}`,
	)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected JWT, got %q", token)
	}
	if !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		t.Fatal("gss match token signature did not verify")
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "ES256" || header["kid"] == "" {
		t.Fatalf("unexpected header: %#v", header)
	}
	if _, exists := header["jku"]; exists {
		t.Fatalf("public-match gss header must not contain jku: %#v", header)
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Issuer   string `json:"iss"`
		Subject  string `json:"sub"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
		Gamesync struct {
			GSID string `json:"gsid"`
			USID string `json:"usid"`
			TID  string `json:"tid"`
			UID  string `json:"uid"`
			Team string `json:"team"`
			Type int    `json:"typ"`
		} `json:"gamesync"`
	}
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Issuer != "gss" || payload.Subject != "u-1800000001" || payload.Expires-payload.IssuedAt != 3600 {
		t.Fatalf("unexpected top-level claims: %+v", payload)
	}
	if payload.Gamesync.GSID != "gs-123" || payload.Gamesync.USID != "us-456" || payload.Gamesync.TID != nplnTenantID || payload.Gamesync.UID != payload.Subject || payload.Gamesync.Team != "Player" || payload.Gamesync.Type != 1 {
		t.Fatalf("unexpected gamesync claims: %+v", payload.Gamesync)
	}
}
