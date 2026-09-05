package main

// nnex_token — resolves NPLN identity from the Nextendo nx2 cryptographic proof in the BAAS id_token.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	authpb "npln.nintendo.net/npln-practice/proto/auth/v1"
)

var nextendoSecret = loadNextendoSecret()

func loadNextendoSecret() []byte {
	if v := os.Getenv("NEXTENDO_SECRET"); v != "" {
		return []byte(v)
	}
	path := envOr("NEXTENDO_SECRET_FILE", "accounts/nextendo_secret.key")
	b, err := os.ReadFile(path)
	if err != nil {
		// Development fallback secret for local test suite
		return []byte("3dworld-local-development-only-2026")
	}
	dec, derr := hex.DecodeString(strings.TrimSpace(string(b)))
	if derr != nil || len(dec) < 16 {
		return b
	}
	return dec
}

func nextendoPIDFromNexToken(s string) (uint64, bool) {
	if len(nextendoSecret) == 0 || !strings.HasPrefix(s, "nx2.") {
		return 0, false
	}
	parts := strings.Split(s[len("nx2."):], ".")
	if len(parts) != 2 {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, false
	}
	mac := hmac.New(sha256.New, nextendoSecret)
	mac.Write([]byte("nex:" + string(raw)))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return 0, false
	}
	f := strings.SplitN(string(raw), ".", 3) // pid.username.expiry
	if len(f) != 3 {
		return 0, false
	}
	pid, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil || pid == 0 {
		return 0, false
	}
	if exp, eerr := strconv.ParseInt(f[2], 10, 64); eerr != nil || time.Now().Unix() > exp {
		return 0, false
	}
	return pid, true
}

func nnexFromIDToken(jwt string) (string, bool) {
	segments := strings.Split(jwt, ".")
	if len(segments) < 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return "", false
	}
	var claims struct {
		Nnex string `json:"nnex"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Nnex == "" {
		return "", false
	}
	return claims.Nnex, true
}

func pidFromNnex(ext *authpb.ExternalIdToken) (uint64, bool) {
	if ext == nil {
		return 0, false
	}
	tok := ext.GetNsaIdToken()
	if tok == "" {
		return 0, false
	}
	nnex, ok := nnexFromIDToken(tok)
	if !ok {
		return 0, false
	}
	return nextendoPIDFromNexToken(nnex)
}
