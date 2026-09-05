package main

import "testing"

func TestFriendUserAllowsBidirectionalPresence(t *testing.T) {
	got := friendUser("u-me", nplnFriendData{
		UserID:     "u-friend",
		AccountHex: "0123456789abcdef",
	})

	if got.GetName() != nplnTenant+"/users/u-me/friendUsers/u-friend" {
		t.Fatalf("name = %q", got.GetName())
	}
	if got.GetFriendUser() != nplnTenant+"/users/u-friend" {
		t.Fatalf("friend_user = %q", got.GetFriendUser())
	}
	if got.GetNsaId() != "0123456789abcdef" {
		t.Fatalf("nsa_id = %q", got.GetNsaId())
	}
	relationship := got.GetRelationship()
	if relationship == nil {
		t.Fatal("relationship is nil")
	}
	if !relationship.GetPresenceDeliverable() {
		t.Fatal("presence_deliverable = false")
	}
	if !relationship.GetPresenceReceivable() {
		t.Fatal("presence_receivable = false")
	}
}
