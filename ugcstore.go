package main

// ugcstore implements the query family observed after Wonder finishes a
// course. The remaining UGC methods stay explicitly Unimplemented until the
// client demonstrates their request and response contracts.

import (
	"log"
	"strings"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	ugcpb "npln.nintendo.net/npln-practice/proto/ugcstore/v1"
)

type ugcstoreServer struct {
	ugcpb.UnimplementedUgcstoreServer
}

func observedWonderCourseParent(parent string) bool {
	parent = strings.Trim(parent, "/")
	var documentPath string
	for _, prefix := range []string{"tenants/current/documents/", nplnTenant + "/documents/"} {
		if strings.HasPrefix(parent, prefix) {
			documentPath = strings.TrimPrefix(parent, prefix)
			break
		}
	}
	parts := strings.Split(documentPath, "/")
	if len(parts) != 6 || parts[0] != "ke" || parts[2] != "sdv" || parts[4] != "sh" {
		return false
	}
	for _, value := range []string{parts[1], parts[3], parts[5]} {
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return !unicode.IsDigit(r) }) != -1 {
			return false
		}
	}
	return true
}

func observedWonderCourseQuery(req *ugcpb.RunQueryRequest) bool {
	if req == nil || !observedWonderCourseParent(req.GetParent()) || req.GetRuleContext() != nil {
		return false
	}
	query := req.GetStructuredQuery()
	if query == nil || query.GetSelect() != nil || query.GetWhere() != nil || query.GetOffset() != 0 ||
		query.GetStartAt() != nil || query.GetEndAt() != nil || query.GetLimit() == nil || query.GetLimit().GetValue() != 30 {
		return false
	}
	from := query.GetFrom()
	if len(from) != 1 || from[0].GetCollectionId() != "ku" || from[0].GetAllDescendants() {
		return false
	}
	order := query.GetOrderBy()
	return len(order) == 1 && order[0].GetField().GetFieldPath() == "`ut`" &&
		order[0].GetDirection() == ugcpb.StructuredQuery_Order_DESCENDING
}

// RunQuery acknowledges the exact course UGC query captured from both local
// clients. The local server has no UGC documents yet, so it returns Firestore's
// documented empty-result marker: one response with read_time and no document.
func (u *ugcstoreServer) RunQuery(req *ugcpb.RunQueryRequest, stream grpc.ServerStreamingServer[ugcpb.RunQueryResponse]) error {
	claims, err := authenticatedNplnCaller(stream.Context())
	if err != nil {
		log.Printf("[NPLN Ugcstore] RunQuery rejected parent=%q: %v", req.GetParent(), err)
		return status.Error(codes.Unauthenticated, "invalid caller authorization")
	}
	if metadataUID := uidFromCtx(stream.Context()); metadataUID != "" && metadataUID != claims.Subject {
		log.Printf("[NPLN Ugcstore] RunQuery rejected uid=%q subject=%q", metadataUID, claims.Subject)
		return status.Error(codes.PermissionDenied, "request uid does not match caller")
	}
	if !observedWonderCourseParent(req.GetParent()) {
		return status.Error(codes.InvalidArgument, "invalid Wonder course query parent")
	}
	if !observedWonderCourseQuery(req) {
		return status.Error(codes.Unimplemented, "unobserved UGC query")
	}
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	readTime := timestamppb.Now()
	if err := stream.Send(&ugcpb.RunQueryResponse{ReadTime: readTime}); err != nil {
		return err
	}
	log.Printf("[NPLN Ugcstore] RunQuery collection=%q parent=%q subject=%q results=0 read_time=%s",
		"ku", req.GetParent(), claims.Subject, readTime.AsTime().UTC().Format("2006-01-02T15:04:05.000000000Z"))
	return nil
}
