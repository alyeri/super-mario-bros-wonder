package main

// subsdk_token — decode identity token from hardware console or subsdk payload.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
)

var subsdkAuthSecret = os.Getenv("NPLN_AUTH_HMAC_SECRET")

func autoriserJetonsNonVerifies() bool {
	return os.Getenv("NPLN_ALLOW_UNVERIFIED") == "1"
}

type subsdkClaims struct {
	Timestamp       int64  `json:"timestamp"`
	TenantID        string `json:"tenant_id"`
	Emulator        bool   `json:"emulator"`
	DisplayVersion  string `json:"display_version"`
	NextendoVersion string `json:"nextendo_version"`
	PseudoID        string `json:"pseudo_id"`
	AccountBytes    string `json:"account_bytes"`
	FestSHA         string `json:"fest_config_sha256"`
	UserSHA         string `json:"user_config_sha256"`
}

func parseSubsdkToken(tok string) (*subsdkClaims, bool) {
	i := strings.IndexByte(tok, ',')
	if i <= 0 || i+1 >= len(tok) {
		return nil, false
	}
	b64, mac := tok[:i], tok[i+1:]
	if subsdkAuthSecret == "" && !autoriserJetonsNonVerifies() {
		return nil, false
	}
	if subsdkAuthSecret != "" {
		h := hmac.New(sha256.New, []byte(subsdkAuthSecret))
		h.Write([]byte(b64))
		want := hex.EncodeToString(h.Sum(nil))
		if !hmac.Equal([]byte(strings.ToLower(mac)), []byte(want)) {
			return nil, false
		}
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, false
	}
	var c subsdkClaims
	if json.Unmarshal(raw, &c) != nil {
		return nil, false
	}
	return &c, true
}

func pidFromAccountBytes(h string) uint64 {
	b, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil || len(b) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(b[:8])
}
