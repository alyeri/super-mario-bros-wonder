package main

// messaging implements the generic NPLN receive stream demonstrated by Wonder.
// Message delivery, sending, and acknowledgements remain unimplemented until
// the client exercises them.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	msgpb "npln.nintendo.net/npln-practice/proto/messaging/v1"
)

type nplnCallerClaims struct {
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	NPLN      struct {
		TenantID string `json:"tid"`
	} `json:"npln"`
}

func authenticatedNplnCaller(ctx context.Context) (*nplnCallerClaims, error) {
	token, err := bearerToken(ctx)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !verifyNplnAccessToken(parts[0], parts[1], parts[2]) {
		return nil, fmt.Errorf("invalid ES256 signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var claims nplnCallerClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	if claims.Issuer != nplnIssuer || claims.Subject == "" || claims.ExpiresAt <= time.Now().Unix() {
		return nil, fmt.Errorf("invalid or expired caller claims")
	}
	if claims.NPLN.TenantID != nplnTenantID {
		return nil, fmt.Errorf("wrong tenant")
	}
	return &claims, nil
}

func messagingUserMatches(requestUser, uid string) bool {
	return requestUser == "tenants/current/users/current" ||
		requestUser == nplnTenant+"/users/"+uid ||
		requestUser == "tenants/current/users/"+uid
}

type messagingServer struct {
	msgpb.UnimplementedMessagingServer
}

func messagingKeepAlive(idleTimeout time.Duration) *msgpb.RecvMessageResponse {
	return &msgpb.RecvMessageResponse{
		Reply: &msgpb.RecvMessageResponse_KeepAlive{
			KeepAlive: &msgpb.KeepAlive{IdleTimeout: durationpb.New(idleTimeout)},
		},
	}
}

// RecvMessage immediately establishes the NPLN messaging stream with its
// advertised idle timeout, then keeps it alive while no game messages are
// pending. The initial 95-second timeout and the 45-second cadence are part of
// the protocol contract; without them Wonder repeatedly abandons the stream.
func (m *messagingServer) RecvMessage(req *msgpb.RecvMessageRequest, stream grpc.ServerStreamingServer[msgpb.RecvMessageResponse]) error {
	claims, err := authenticatedNplnCaller(stream.Context())
	if err != nil {
		log.Printf("[NPLN Messaging] RecvMessage rejected user=%q: %v", req.GetUser(), err)
		return status.Error(codes.Unauthenticated, "invalid caller authorization")
	}
	if !messagingUserMatches(req.GetUser(), claims.Subject) {
		log.Printf("[NPLN Messaging] RecvMessage rejected mismatched user=%q subject=%q", req.GetUser(), claims.Subject)
		return status.Error(codes.PermissionDenied, "requested user does not match caller")
	}
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	if err := stream.Send(messagingKeepAlive(95 * time.Second)); err != nil {
		return err
	}
	log.Printf("[NPLN Messaging] RecvMessage stream opened user=%q subject=%q idle_timeout=95s", req.GetUser(), claims.Subject)

	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			log.Printf("[NPLN Messaging] RecvMessage stream closed user=%q", req.GetUser())
			return nil
		case <-ticker.C:
			if err := stream.Send(messagingKeepAlive(0)); err != nil {
				return err
			}
		}
	}
}
