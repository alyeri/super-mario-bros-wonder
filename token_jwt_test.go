package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestMintAndVerifyNplnAccessToken(t *testing.T) {
	pid := uint64(1800000001)
	userPath := "tenants/t-ba973ec6-lp1/users/u-wonderdev1800000001"
	tenant := "tenants/t-ba973ec6-lp1"

	token := mintNplnAccessToken(pid, userPath, tenant)
	if token == "" {
		t.Fatalf("mintNplnAccessToken returned empty token")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	// Verify JWT claims
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("failed to base64 decode JWT payload: %v", err)
	}

	var claims struct {
		Sub  string `json:"sub"`
		Npln struct {
			AppID         string `json:"app_id"`
			TID           string `json:"tid"`
			ExtID         string `json:"ext_id"`
			Authorization struct {
				Allow         []string `json:"allow"`
				NsoRestricted bool     `json:"nso_restricted"`
			} `json:"authorization"`
		} `json:"npln"`
	}

	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		t.Fatalf("failed to unmarshal claims: %v", err)
	}

	if claims.Npln.AppID != "010015100B514000" {
		t.Errorf("expected app_id 010015100B514000, got %s", claims.Npln.AppID)
	}

	if claims.Npln.TID != "t-ba973ec6-lp1" {
		t.Errorf("expected tid t-ba973ec6-lp1, got %s", claims.Npln.TID)
	}

	if claims.Npln.ExtID != "000000006b49d201" { // hex(1800000001)
		t.Errorf("expected ext_id 000000006b49d201, got %s", claims.Npln.ExtID)
	}

	if claims.Npln.Authorization.NsoRestricted {
		t.Errorf("expected nso_restricted to be false")
	}

	// Verify cryptographic signature
	if !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		t.Fatalf("verifyNplnAccessToken failed to verify newly minted token")
	}
}

func TestMintSessionToken(t *testing.T) {
	uid := "u-wonderdev1800000001"
	tenant := "tenants/t-ba973ec6-lp1"
	gsName := tenant + "/gameSessions/gs-12345"
	userSess := gsName + "/userSessions/us-67890"

	token := mintSessionToken(uid, tenant, gsName, userSess)
	if token == "" || strings.HasPrefix(token, "nextendo-gss.fallback") {
		t.Fatalf("failed to mint session token")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts in session token JWT, got %d", len(parts))
	}

	if !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		t.Fatalf("failed to verify session token signature")
	}
}
