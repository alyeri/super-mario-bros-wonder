package main

// friends — nn.npln.friends.v1.Friends & PresenceService for Super Mario Bros. Wonder.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	friendspb "npln.nintendo.net/npln-practice/proto/friends/v1"
)

func callerPID(ctx context.Context) (uint64, bool) {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, a := range md.Get("authorization") {
		a = strings.TrimSpace(a)
		a = strings.TrimPrefix(a, "Bearer ")
		a = strings.TrimPrefix(a, "bearer ")

		if pid, ok := pidFromJWT(a); ok {
			return pid, true
		}

		if allowUnverified() {
			const pfx = "nextendo-npln-access."
			if strings.HasPrefix(a, pfx) {
				seg := strings.SplitN(a[len(pfx):], ".", 2)[0]
				if pid, err := strconv.ParseUint(seg, 10, 64); err == nil {
					return pid, true
				}
			}
		}
	}
	return 0, false
}

func pidFromJWT(tok string) (uint64, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "ey") {
		return 0, false
	}
	if !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		Npln struct {
			ExtID string `json:"ext_id"`
		} `json:"npln"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Npln.ExtID == "" {
		return 0, false
	}
	pid, err := strconv.ParseUint(claims.Npln.ExtID, 16, 64)
	if err != nil {
		return 0, false
	}
	return pid, true
}

func uidFromCtx(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("uid"); len(v) > 0 {
		return v[0]
	}
	return ""
}

type friendsServer struct {
	friendspb.UnimplementedFriendsServer
}

func (s *friendsServer) ActivateUser(ctx context.Context, req *friendspb.ActivateUserRequest) (*friendspb.ActivateUserResponse, error) {
	log.Printf("[NPLN Friends] ActivateUser %q", req.GetName())
	return &friendspb.ActivateUserResponse{}, nil
}

func (s *friendsServer) ListBlockingUsers(ctx context.Context, req *friendspb.ListBlockingUsersRequest) (*friendspb.ListBlockingUsersResponse, error) {
	log.Printf("[NPLN Friends] ListBlockingUsers parent=%q (empty)", req.GetParent())
	return &friendspb.ListBlockingUsersResponse{}, nil
}

func friendUser(myUID string, f nplnFriendData) *friendspb.FriendUser {
	return &friendspb.FriendUser{
		Name:       nplnTenant + "/users/" + myUID + "/friendUsers/" + f.UserID,
		FriendUser: nplnTenant + "/users/" + f.UserID,
		NsaId:      f.AccountHex,
		Relationship: &friendspb.FriendUser_Relationship{
			PresenceDeliverable: true,
			PresenceReceivable:  true,
		},
	}
}

func (s *friendsServer) ListFriendUsers(ctx context.Context, req *friendspb.ListFriendUsersRequest) (*friendspb.ListFriendUsersResponse, error) {
	pid, ok := callerPID(ctx)
	if !ok {
		return &friendspb.ListFriendUsersResponse{}, nil
	}
	me, err := accountFriends(pid)
	if err != nil {
		log.Printf("[NPLN Friends] ListFriendUsers pid=%d: %v", pid, err)
		return &friendspb.ListFriendUsersResponse{}, nil
	}
	users := make([]*friendspb.FriendUser, 0, len(me.Friends))
	for _, f := range me.Friends {
		users = append(users, friendUser(me.UserID, f))
	}
	return &friendspb.ListFriendUsersResponse{FriendUsers: users}, nil
}

func (s *friendsServer) SubscribeFriendUsers(req *friendspb.SubscribeFriendUsersRequest, stream grpc.ServerStreamingServer[friendspb.SubscribeFriendUsersResponse]) error {
	ctx := stream.Context()
	ka := durationpb.New(30 * time.Second)

	pid, ok := callerPID(ctx)
	if !ok {
		log.Printf("[NPLN Friends] SubscribeFriendUsers: NO PID -> sending empty list")
		_ = stream.SendMsg(&friendspb.SubscribeFriendUsersResponse{KeepAliveInterval: ka})
		<-ctx.Done()
		return nil
	}

	me, err := accountFriends(pid)
	if err != nil {
		log.Printf("[NPLN Friends] Subscribe pid=%d: %v", pid, err)
		_ = stream.SendMsg(&friendspb.SubscribeFriendUsersResponse{KeepAliveInterval: ka})
	} else {
		accounts := make([]*friendspb.SubscribeFriendUsersResponse_FriendAccount, 0, len(me.Friends))
		for _, f := range me.Friends {
			compte := &friendspb.SubscribeFriendUsersResponse_FriendAccount{
				NsaId: f.AccountHex,
				Users: []*friendspb.FriendUser{friendUser(me.UserID, f)},
			}
			accounts = append(accounts, compte)
		}
		resp := &friendspb.SubscribeFriendUsersResponse{
			FriendAccounts:    accounts,
			KeepAliveInterval: ka,
		}
		log.Printf("[NPLN Friends] Subscribe pid=%d -> %d friend(s)", pid, len(accounts))
		_ = stream.SendMsg(resp)
	}

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := stream.SendMsg(&friendspb.SubscribeFriendUsersResponse{KeepAliveInterval: ka}); err != nil {
				return err
			}
		}
	}
}

// Presence Server
type presenceServer struct {
	friendspb.UnimplementedPresenceServiceServer
}

var presenceHeartbeatExtra = protoreflect.RawFields([]byte{0x12, 0x02, 0x08, 0x32})

func presenceHeartbeat() *friendspb.Heartbeat {
	hb := &friendspb.Heartbeat{Interval: durationpb.New(30 * time.Second)}
	hb.ProtoReflect().SetUnknown(presenceHeartbeatExtra)
	return hb
}

func (p *presenceServer) KeepAlive(stream grpc.BidiStreamingServer[friendspb.KeepAliveRequest, friendspb.KeepAliveResponse]) error {
	ctx := stream.Context()
	log.Printf("[NPLN Presence] KeepAlive stream opened")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = stream.SendMsg(&friendspb.KeepAliveResponse{
				Heartbeat: presenceHeartbeat(),
			})
		}
	}
}

func (p *presenceServer) SubscribePresences(req *friendspb.SubscribePresencesRequest, stream grpc.ServerStreamingServer[friendspb.SubscribePresencesResponse]) error {
	ctx := stream.Context()
	log.Printf("[NPLN Presence] SubscribePresences user=%q", req.GetUser())

	// Send initial heartbeat
	_ = stream.SendMsg(&friendspb.SubscribePresencesResponse{
		Response: &friendspb.SubscribePresencesResponse_Heartbeat{Heartbeat: presenceHeartbeat()},
	})

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = stream.SendMsg(&friendspb.SubscribePresencesResponse{
				Response: &friendspb.SubscribePresencesResponse_Heartbeat{Heartbeat: presenceHeartbeat()},
			})
		}
	}
}
