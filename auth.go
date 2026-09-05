package main

// auth — nn.npln.auth.v1.Auth for Super Mario Bros. Wonder.
// Handles entry-point authentication (IssuePrearrangedUserToken, IssueToken, RefreshToken).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	authpb "npln.nintendo.net/npln-practice/proto/auth/v1"
)

type authServer struct {
	authpb.UnimplementedAuthServer
}

func short(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

func describe(t *authpb.ExternalIdToken) string {
	if t == nil {
		return "<none>"
	}
	if v := t.GetNsaIdToken(); v != "" {
		return "nsa:" + short(v)
	}
	if v := t.GetDummyExtIdToken(); v != "" {
		return "dummy:" + v
	}
	return "<empty>"
}

func allowUnverified() bool { return os.Getenv("NPLN_ALLOW_UNVERIFIED") != "" }

func nsaFromExternal(ext *authpb.ExternalIdToken) (string, bool) {
	if ext == nil {
		return "", false
	}
	if v := ext.GetDummyExtIdToken(); v != "" {
		return v, true
	}
	jwt := ext.GetNsaIdToken()
	if jwt == "" {
		return "", false
	}
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return "", false
	}
	for _, k := range []string{"sub", "nsa", "nsa_id", "san", "user_id"} {
		if s, ok := claims[k].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

func tenantOr(t string) string {
	if t == "" {
		return nplnTenant
	}
	return t
}

func gatedIdentity(ext *authpb.ExternalIdToken, tenant string) (uint64, string, error) {
	tenant = tenantOr(tenant)

	fallback := func(why string) (uint64, string, error) {
		// In dev/local mode default to standard Dev Player (PID 1800000001)
		devPID := uint64(1800000001)
		userPath := tenant + "/users/u-3dworlddev" + strconv.FormatUint(devPID, 10)
		log.Printf("[NPLN Auth] Dev fallback identity (%s) -> %s (pid=%d)", why, userPath, devPID)
		return devPID, userPath, nil
	}

	// 1. Check subsdk token
	if ext != nil {
		if tok := ext.GetNsaIdToken(); tok != "" {
			if c, ok := parseSubsdkToken(tok); ok {
				if pid := pidFromAccountBytes(c.AccountBytes); pid != 0 {
					log.Printf("[NPLN Auth] subsdk token pid=%d", pid)
					acc, err := accountFriends(pid)
					if err == nil {
						return pid, tenant + "/users/" + acc.UserID, nil
					}
					return pid, tenant + "/users/u-" + strconv.FormatUint(pid, 10), nil
				}
			}
		}
	}

	// 2. Check nnex token inside BAAS JWT
	if pid, ok := pidFromNnex(ext); ok {
		log.Printf("[NPLN Auth] nnex verified pid=%d", pid)
		acc, err := accountFriends(pid)
		if err == nil {
			return pid, tenant + "/users/" + acc.UserID, nil
		}
		return pid, tenant + "/users/u-" + strconv.FormatUint(pid, 10), nil
	}

	// 3. Check NSA id token
	if nsa, ok := nsaFromExternal(ext); ok {
		if pid, err := resolveNSAToPID(nsa); err == nil && pid != 0 {
			log.Printf("[NPLN Auth] NSA resolved -> pid=%d", pid)
			acc, err := accountFriends(pid)
			if err == nil {
				return pid, tenant + "/users/" + acc.UserID, nil
			}
			return pid, tenant + "/users/u-" + strconv.FormatUint(pid, 10), nil
		}
	}

	return fallback("no matching local token found")
}

var identitesParUid = struct {
	sync.Mutex
	m map[string]uint64
}{m: map[string]uint64{}}

func retenirIdentite(userPath string, pid uint64) {
	uid := userPath
	if i := strings.LastIndexByte(uid, '/'); i >= 0 {
		uid = uid[i+1:]
	}
	if uid == "" || pid == 0 {
		return
	}
	identitesParUid.Lock()
	identitesParUid.m[uid] = pid
	identitesParUid.Unlock()
}

func pidPourUid(uid string) uint64 {
	identitesParUid.Lock()
	defer identitesParUid.Unlock()
	return identitesParUid.m[uid]
}

func newTokenPID(pid uint64, userPath string) *authpb.Token {
	retenirIdentite(userPath, pid)

	return &authpb.Token{
		User:         userPath,
		AccessToken:  mintNplnAccessToken(pid, userPath, nplnTenant),
		RefreshToken: jetonRafraichissement(pid),
		Ttl:          durationpb.New(nplnTokenTTL),
	}
}

func jetonRafraichissement(pid uint64) string {
	corps := fmt.Sprintf("nextendo-npln-refresh.%d", pid)
	mac := hmac.New(sha256.New, loadNextendoSecret())
	mac.Write([]byte(corps))
	return corps + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func pidDuJetonRafraichissement(tok string) (uint64, bool) {
	i := strings.LastIndexByte(tok, '.')
	if i <= 0 {
		return 0, false
	}
	corps, sig := tok[:i], tok[i+1:]
	pid, err := strconv.ParseUint(strings.TrimPrefix(corps, "nextendo-npln-refresh."), 10, 64)
	if err != nil || pid == 0 || !strings.HasPrefix(corps, "nextendo-npln-refresh.") {
		return 0, false
	}
	mac := hmac.New(sha256.New, loadNextendoSecret())
	mac.Write([]byte(corps))
	attendu := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(attendu), []byte(sig)) {
		return 0, false
	}
	return pid, true
}

func (s *authServer) CreateUser(ctx context.Context, req *authpb.CreateUserRequest) (*authpb.User, error) {
	pid, userPath, err := gatedIdentity(req.GetExternalIdToken(), req.GetParent())
	if err != nil {
		return nil, err
	}
	log.Printf("[NPLN Auth] CreateUser parent=%q pid=%d -> %s", req.GetParent(), pid, userPath)
	return &authpb.User{Name: userPath, Account: req.GetParent(), ShortId: 1}, nil
}

func (s *authServer) IssueToken(ctx context.Context, req *authpb.IssueTokenRequest) (*authpb.IssueTokenResponse, error) {
	pid, userPath, err := gatedIdentity(req.GetExternalIdToken(), "")
	if err != nil {
		log.Printf("[NPLN Auth] IssueToken DENIED ext=%s: %v", describe(req.GetExternalIdToken()), err)
		return nil, err
	}
	log.Printf("[NPLN Auth] IssueToken pid=%d user=%s", pid, userPath)
	return &authpb.IssueTokenResponse{Token: newTokenPID(pid, userPath)}, nil
}

func (s *authServer) RefreshToken(ctx context.Context, req *authpb.RefreshTokenRequest) (*authpb.RefreshTokenResponse, error) {
	pid, ok := callerPID(ctx)
	if !ok || pid == 0 {
		pid, ok = pidDuJetonRafraichissement(req.GetRefreshToken())
	}
	if !ok || pid == 0 {
		log.Printf("[NPLN Auth] RefreshToken DENIED: no proven identity (user=%q)", req.GetUser())
		return nil, status.Error(codes.Unauthenticated, "refresh token without a proven identity")
	}
	log.Printf("[NPLN Auth] RefreshToken pid=%d user=%s", pid, req.GetUser())
	return &authpb.RefreshTokenResponse{Token: newTokenPID(pid, req.GetUser())}, nil
}

func (s *authServer) IssuePrearrangedUserToken(ctx context.Context, req *authpb.IssuePrearrangedUserTokenRequest) (*authpb.IssuePrearrangedUserTokenResponse, error) {
	pid, userPath, err := gatedIdentity(req.GetExternalIdToken(), req.GetTenant())
	if err != nil {
		log.Printf("[NPLN Auth] IssuePrearrangedUserToken DENIED ext=%s: %v", describe(req.GetExternalIdToken()), err)
		return nil, err
	}
	user := &authpb.User{Name: userPath, ShortId: int64(req.GetUserIndex())}
	log.Printf("[NPLN Auth] IssuePrearrangedUserToken SUCCESS tenant=%q pid=%d user=%s user_index=%d",
		req.GetTenant(), pid, userPath, req.GetUserIndex())
	return &authpb.IssuePrearrangedUserTokenResponse{User: user, Token: newTokenPID(pid, userPath)}, nil
}

func (s *authServer) IssueAnonymousUserToken(ctx context.Context, req *authpb.IssueAnonymousUserTokenRequest) (*authpb.IssueAnonymousUserTokenResponse, error) {
	pid, userPath, err := gatedIdentity(req.GetExternalIdToken(), req.GetTenant())
	if err != nil {
		log.Printf("[NPLN Auth] IssueAnonymousUserToken DENIED: %v", err)
		return nil, err
	}
	log.Printf("[NPLN Auth] IssueAnonymousUserToken pid=%d user=%s", pid, userPath)
	return &authpb.IssueAnonymousUserTokenResponse{Token: newTokenPID(pid, userPath)}, nil
}

func (s *authServer) ValidateToken(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	pid, ok := callerPID(ctx)
	if !ok || pid == 0 {
		log.Printf("[NPLN Auth] ValidateToken anonymous/fallback -> OK")
		return &emptypb.Empty{}, nil
	}
	log.Printf("[NPLN Auth] ValidateToken pid=%d -> OK", pid)
	return &emptypb.Empty{}, nil
}

func (s *authServer) DeleteUser(ctx context.Context, req *authpb.DeleteUserRequest) (*emptypb.Empty, error) {
	log.Printf("[NPLN Auth] DeleteUser name=%q", req.GetName())
	return &emptypb.Empty{}, nil
}
