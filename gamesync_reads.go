package main

import (
	"context"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	gspb "npln.nintendo.net/npln-practice/proto/gamesync/v1"
)

func (g *gamesyncServer) ReadDocuments(ctx context.Context, req *gspb.ReadDocumentsRequest) (*gspb.ReadDocumentsResponse, error) {
	session, err := g.authorizedSession(ctx, "")
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid session authorization")
	}
	if len(req.GetTransaction()) != 0 {
		return nil, status.Error(codes.Unimplemented, "transactional reads not supported")
	}
	if len(req.GetDocuments()) > 256 {
		return nil, status.Error(codes.ResourceExhausted, "too many documents")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := &gspb.ReadDocumentsResponse{ReadTime: timestamppb.Now()}
	for _, name := range req.GetDocuments() {
		if _, _, ok := documentPath(name); !ok {
			return nil, status.Error(codes.InvalidArgument, "invalid document name")
		}
		d := g.documents[session.GameSession][name]
		if d == nil {
			out.Results = append(out.Results, &gspb.ReadResult{Result: &gspb.ReadResult_Missing{Missing: name}})
		} else {
			masked, err := maskedDocument(d, req.ReadMask)
			if err != nil {
				return nil, err
			}
			out.Results = append(out.Results, &gspb.ReadResult{Result: &gspb.ReadResult_Found{Found: masked}})
		}
	}
	return out, nil
}

func (g *gamesyncServer) ListDocuments(ctx context.Context, req *gspb.ListDocumentsRequest) (*gspb.ListDocumentsResponse, error) {
	session, err := g.authorizedSession(ctx, "")
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid session authorization")
	}
	if req == nil || !strings.HasPrefix(req.GetParent(), "docs/") || strings.Count(req.GetParent(), "/") != 1 || len(req.GetParent()) <= 5 || req.GetPageSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid collection or page size")
	}
	if req.ShowMissing {
		return nil, status.Error(codes.Unimplemented, "missing-document enumeration not supported")
	}
	if req.PageToken != "" && !strings.HasPrefix(req.PageToken, req.Parent+"/") {
		return nil, status.Error(codes.InvalidArgument, "page token is outside collection")
	}
	limit := int(req.PageSize)
	if limit == 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	names := []string{}
	for name := range g.documents[session.GameSession] {
		if strings.HasPrefix(name, req.Parent+"/") && strings.Count(name, "/") == 2 && name > req.PageToken {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := &gspb.ListDocumentsResponse{}
	for i, name := range names {
		if i == limit {
			out.NextPageToken = names[i-1]
			break
		}
		d, err := maskedDocument(g.documents[session.GameSession][name], req.ReadMask)
		if err != nil {
			return nil, err
		}
		out.Documents = append(out.Documents, d)
	}
	return out, nil
}

// Both explicit document targets and collections read this same committed
// store; no second synthesized snapshot can disagree with GetDocument.
func (g *gamesyncServer) committedSnapshot(session gamesyncSession, target *gspb.Target) []*gspb.Document {
	g.mu.RLock()
	defer g.mu.RUnlock()
	names := []string{}
	for name := range g.documents[session.GameSession] {
		if targetMatchesDocument(target, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]*gspb.Document, 0, len(names))
	for _, name := range names {
		out = append(out, proto.Clone(g.documents[session.GameSession][name]).(*gspb.Document))
	}
	return out
}
