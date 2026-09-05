package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
)

// One registry is injected into all three services by buildServer. Session
// pointers and their membership are accessed only while mu is held.
type sessionRegistry struct {
	mu         sync.Mutex
	sessions   map[string]*mmpb.GameSession
	pooled     map[string]*publicMatchSession
	friendUIDs func(context.Context, string) (map[string]bool, error)
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]*mmpb.GameSession), pooled: make(map[string]*publicMatchSession), friendUIDs: canonicalFriendUIDs}
}

func chooseRegistry(registries []*sessionRegistry) *sessionRegistry {
	if len(registries) > 0 && registries[0] != nil {
		return registries[0]
	}
	return newSessionRegistry()
}

// Metadata uid is a routing hint, not proof of identity. Require the signed,
// unexpired tenant access token at matchmaking RPC boundaries.
func authenticatedNPLNUID(ctx context.Context) (string, error) {
	token, err := bearerToken(ctx)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "NPLN bearer token required")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		return "", status.Error(codes.Unauthenticated, "invalid token signature")
	}
	var claims struct {
		Subject string `json:"sub"`
		Issuer  string `json:"iss"`
		Expires int64  `json:"exp"`
		NPLN    struct {
			Tenant string `json:"tid"`
		} `json:"npln"`
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Subject == "" || claims.Issuer != nplnIssuer || claims.Expires <= time.Now().Unix() || claims.NPLN.Tenant != nplnTenantID {
		return "", status.Error(codes.Unauthenticated, "invalid or expired tenant access token")
	}
	if hinted := uidFromCtx(ctx); hinted != "" && hinted != claims.Subject {
		return "", status.Error(codes.PermissionDenied, "uid metadata does not match bearer subject")
	}
	return claims.Subject, nil
}

func canonicalFriendUIDs(ctx context.Context, uid string) (map[string]bool, error) {
	pid, ok := callerPID(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "account binding required")
	}
	account, err := accountFriends(pid)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "friend service unavailable")
	}
	if account.UserID != uid {
		return nil, status.Error(codes.PermissionDenied, "account identity mismatch")
	}
	friends := map[string]bool{uid: true}
	for _, friend := range account.Friends {
		friends[friend.UserID] = true
	}
	return friends, nil
}

func sessionHasUID(session *mmpb.GameSession, uid string) bool {
	for _, u := range session.GetUserSessions() {
		if userIDFromPath(u.GetUser()) == uid && u.GetState() == mmpb.UserSession_ACTIVE {
			return true
		}
	}
	return false
}

func (r *sessionRegistry) member(gameSession, userSession, uid string) (*mmpb.GameSession, *mmpb.UserSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gs := r.sessions[lastResourceSegment(gameSession)]
	for _, u := range gs.GetUserSessions() {
		if lastResourceSegment(u.Name) == lastResourceSegment(userSession) && userIDFromPath(u.User) == uid && u.State == mmpb.UserSession_ACTIVE {
			return proto.Clone(gs).(*mmpb.GameSession), proto.Clone(u).(*mmpb.UserSession)
		}
	}
	return nil, nil
}

func (r *sessionRegistry) depart(gameSession, userSession string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := lastResourceSegment(gameSession)
	gs := r.sessions[id]
	if gs == nil {
		return
	}
	users := gs.UserSessions[:0]
	for _, u := range gs.UserSessions {
		if lastResourceSegment(u.Name) != lastResourceSegment(userSession) {
			users = append(users, u)
		}
	}
	gs.UserSessions = users
	gs.CurrentParticipantCount = int32(len(users))
	if pooled := r.pooled[id]; pooled != nil {
		members := pooled.members[:0]
		for _, u := range pooled.members {
			if lastResourceSegment(u.userSession) != lastResourceSegment(userSession) {
				members = append(members, u)
			}
		}
		pooled.members = members
	}
	// Keep the object as a tombstone for old tickets; it is never selectable.
	if len(users) == 0 {
		gs.CanParticipate = false
		gs.State = mmpb.GameSession_TERMINATED
	}
}
