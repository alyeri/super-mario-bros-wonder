package main

// token_jwt — mint a locally signed candidate NPLN token.
//
// The current ES256 shape has not yet been validated against traffic from Wonder:
//   header  {"alg":"ES256","jku":"jwkSets/nplnAccessToken","kid":"<uuid>"}
//   payload {"exp":..., "iat":..., "iss":"default iss", "sub":"u-...",
//            "npln":{"aid":"a-...","app_id":"010015100B514000",
//                    "authorization":{"allow":["**"],"deny":[],"nso_restricted":false},
//                    "ext_id":"...","ext_id_type":1,"tid":"t-ba973ec6-lp1"}}

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nplnJKU      = "jwkSets/nplnAccessToken"
	nplnIssuer   = "default iss"
	nplnAppID    = "010015100B514000" // Super Mario Bros. Wonder Title ID
	nplnTenantID = "t-ba973ec6-lp1"   // Wonder Tenant ID (ba973ec6)
	nplnTenant   = "tenants/" + nplnTenantID
	nplnTokenTTL = 8 * time.Hour // 28800s
	gssTokenTTL  = 1 * time.Hour
)

var (
	jwtOnce sync.Once
	jwtKey  *ecdsa.PrivateKey
	jwtKID  string
)

func nplnSigningKey() (*ecdsa.PrivateKey, string) {
	jwtOnce.Do(func() {
		jwtKID = "ba973ec6-wonder-4992-80bf-5f289ebbf566"
		path := envOr("NPLN_JWT_KEY", "npln_jwt_wonder_es256.key")
		if b, err := os.ReadFile(path); err == nil {
			if blk, _ := pem.Decode(b); blk != nil {
				if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
					jwtKey = k
					return
				}
			}
		}
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			log.Printf("[NPLN Auth] ecdsa key error: %v", err)
			return
		}
		jwtKey = k
		if der, err := x509.MarshalECPrivateKey(k); err == nil {
			if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
				log.Printf("[NPLN Auth] ES256 key not persisted (%v)", err)
			}
		}
	})
	return jwtKey, jwtKID
}

func verifyNplnAccessToken(headerB64, payloadB64, sigB64 string) bool {
	key, _ := nplnSigningKey()
	if key == nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(headerB64 + "." + payloadB64))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(&key.PublicKey, sum[:], r, s)
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func accountIDFromUser(userID string) string {
	body := strings.TrimPrefix(userID, "u-")
	if body == "" {
		return "a-nextendo"
	}
	return "a-a" + body[1:]
}

func userIDFromPath(userPath string) string {
	if i := strings.LastIndex(userPath, "/"); i >= 0 {
		return userPath[i+1:]
	}
	return userPath
}

func mintNplnAccessToken(pid uint64, userPath, tenant string) string {
	key, kid := nplnSigningKey()
	if key == nil {
		return "nextendo-npln-access." + itoa(pid)
	}

	uid := userIDFromPath(userPath)
	if tenant == "" {
		tenant = nplnTenant
	}
	now := time.Now()

	header := map[string]any{"alg": "ES256", "jku": nplnJKU, "kid": kid}
	payload := map[string]any{
		"exp": now.Add(nplnTokenTTL).Unix(),
		"iat": now.Unix(),
		"iss": nplnIssuer,
		"sub": uid,
		"npln": map[string]any{
			"aid":    accountIDFromUser(uid),
			"app_id": nplnAppID,
			"authorization": map[string]any{
				"allow":          []string{"**"},
				"deny":           []string{},
				"nso_restricted": false,
			},
			"ext_id":      hex16(pid),
			"ext_id_type": 1,
			"tid":         strings.TrimPrefix(tenant, "tenants/"),
		},
	}

	hj, _ := json.Marshal(header)
	pj, _ := json.Marshal(payload)
	signing := b64u(hj) + "." + b64u(pj)

	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		log.Printf("[NPLN Auth] sign error: %v", err)
		return "nextendo-npln-access." + itoa(pid)
	}
	sig := make([]byte, 64)
	copyRightAligned(sig[:32], r)
	copyRightAligned(sig[32:], s)

	return signing + "." + b64u(sig)
}

func mintSessionToken(uid, tenant, gsName, userSess string) string {
	key, kid := nplnSigningKey()
	if key == nil {
		return "nextendo-gss.fallback"
	}
	if uid == "" {
		uid = "u-wonderdev1800000001"
	}
	now := time.Now()

	header := map[string]any{"alg": "ES256", "jku": nplnJKU, "kid": kid}
	payload := map[string]any{
		"exp": now.Add(nplnTokenTTL).Unix(),
		"iat": now.Unix(),
		"iss": nplnIssuer,
		"sub": uid,
		"npln": map[string]any{
			"aid":    accountIDFromUser(uid),
			"app_id": nplnAppID,
			"authorization": map[string]any{
				"allow":          []string{"**"},
				"deny":           []string{},
				"nso_restricted": false,
			},
			"ext_id_type": 1,
			"tid":         strings.TrimPrefix(tenant, "tenants/"),
		},
		"gss": map[string]any{
			"game_session": gsName,
			"user_session": userSess,
		},
	}

	hj, _ := json.Marshal(header)
	pj, _ := json.Marshal(payload)
	signing := b64u(hj) + "." + b64u(pj)

	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		log.Printf("[NPLN MM] session token sign error: %v", err)
		return "nextendo-gss.fallback"
	}
	sig := make([]byte, 64)
	copyRightAligned(sig[:32], r)
	copyRightAligned(sig[32:], s)

	return signing + "." + b64u(sig)
}

// mintGssMatchToken issues the matchmaking token form observed in captured
// NPLN public-match responses: a one-hour ES256 JWT with issuer "gss" and a
// gamesync claim. Both public matchmaking and private session creation must
// use this form because Gamesync.IssueToken validates these exact bindings.
func mintGssMatchToken(uid, tenant, gsName, userSess, team, attrJSON, latencyJSON string) string {
	key, kid := nplnSigningKey()
	if key == nil {
		return "nextendo-gss.fallback"
	}
	if uid == "" {
		return "nextendo-gss.fallback"
	}
	if attrJSON == "" {
		attrJSON = "{}"
	}
	if latencyJSON == "" {
		latencyJSON = `{"latencies":{}}`
	}
	now := time.Now()
	header := map[string]any{"alg": "ES256", "kid": kid}
	payload := map[string]any{
		"exp": now.Add(gssTokenTTL).Unix(),
		"iat": now.Unix(),
		"iss": "gss",
		"sub": uid,
		"gamesync": map[string]any{
			"attr": attrJSON,
			"gsid": userIDFromPath(gsName),
			"ltcy": latencyJSON,
			"team": team,
			"tid":  strings.TrimPrefix(tenant, "tenants/"),
			"typ":  1,
			"uid":  uid,
			"usid": userIDFromPath(userSess),
		},
	}

	hj, _ := json.Marshal(header)
	pj, _ := json.Marshal(payload)
	signing := b64u(hj) + "." + b64u(pj)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		log.Printf("[NPLN MM] gss match token sign error: %v", err)
		return "nextendo-gss.fallback"
	}
	sig := make([]byte, 64)
	copyRightAligned(sig[:32], r)
	copyRightAligned(sig[32:], s)
	return signing + "." + b64u(sig)
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

func hex16(v uint64) string { return fmt.Sprintf("%016x", v) }

func copyRightAligned(dst []byte, v *big.Int) {
	b := v.Bytes()
	if len(b) > len(dst) {
		b = b[len(b)-len(dst):]
	}
	copy(dst[len(dst)-len(b):], b)
}
