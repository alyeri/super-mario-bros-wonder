package main

import (
	"context"
	"strings"
	"testing"

	authpb "npln.nintendo.net/npln-practice/proto/auth/v1"
)

func TestIssuePrearrangedUserToken(t *testing.T) {
	srv := &authServer{}
	ctx := context.Background()

	req := &authpb.IssuePrearrangedUserTokenRequest{
		Tenant:    "tenants/t-ba973ec6-lp1",
		UserIndex: 0,
		ExternalIdToken: &authpb.ExternalIdToken{
			Token: &authpb.ExternalIdToken_DummyExtIdToken{
				DummyExtIdToken: "dummy:1800000001",
			},
		},
	}

	resp, err := srv.IssuePrearrangedUserToken(ctx, req)
	if err != nil {
		t.Fatalf("IssuePrearrangedUserToken failed: %v", err)
	}

	if resp.GetUser() == nil {
		t.Fatalf("expected User object in response")
	}

	if !strings.Contains(resp.GetUser().GetName(), "tenants/t-ba973ec6-lp1/users/") {
		t.Errorf("unexpected user name: %s", resp.GetUser().GetName())
	}

	if resp.GetToken() == nil || resp.GetToken().GetAccessToken() == "" {
		t.Fatalf("expected valid AccessToken in response")
	}

	parts := strings.Split(resp.GetToken().GetAccessToken(), ".")
	if len(parts) != 3 {
		t.Fatalf("expected valid 3-part JWT, got: %s", resp.GetToken().GetAccessToken())
	}
}
