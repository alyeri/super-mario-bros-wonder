package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	ugcpb "npln.nintendo.net/npln-practice/proto/ugcstore/v1"
)

type fakeUgcQueryStream struct {
	ctx        context.Context
	headerSent bool
	responses  []*ugcpb.RunQueryResponse
}

func (f *fakeUgcQueryStream) Context() context.Context     { return f.ctx }
func (f *fakeUgcQueryStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeUgcQueryStream) SendHeader(metadata.MD) error { f.headerSent = true; return nil }
func (f *fakeUgcQueryStream) SetTrailer(metadata.MD)       {}
func (f *fakeUgcQueryStream) Send(response *ugcpb.RunQueryResponse) error {
	f.responses = append(f.responses, response)
	return nil
}
func (f *fakeUgcQueryStream) SendMsg(message any) error {
	return f.Send(message.(*ugcpb.RunQueryResponse))
}
func (f *fakeUgcQueryStream) RecvMsg(any) error { return nil }

func capturedWonderCourseQuery() *ugcpb.RunQueryRequest {
	return &ugcpb.RunQueryRequest{
		Parent: "tenants/current/documents/ke/1/sdv/3/sh/2937190396",
		QueryType: &ugcpb.RunQueryRequest_StructuredQuery{StructuredQuery: &ugcpb.StructuredQuery{
			From: []*ugcpb.StructuredQuery_CollectionSelector{{CollectionId: "ku"}},
			OrderBy: []*ugcpb.StructuredQuery_Order{{
				Field:     &ugcpb.StructuredQuery_FieldReference{FieldPath: "`ut`"},
				Direction: ugcpb.StructuredQuery_Order_DESCENDING,
			}},
			Limit: wrapperspb.Int32(30),
		}},
	}
}

func ugcQueryContext(uid string) context.Context {
	accessToken := mintNplnAccessToken(1800000001, nplnTenant+"/users/"+uid, nplnTenant)
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer "+accessToken,
		"npln-tenant-id", nplnTenantID,
		"uid", uid,
	))
}

func TestRunQueryReturnsDocumentedEmptyResultMarkerForCapturedCourseQuery(t *testing.T) {
	stream := &fakeUgcQueryStream{ctx: ugcQueryContext("u-test")}
	if err := (&ugcstoreServer{}).RunQuery(capturedWonderCourseQuery(), stream); err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if !stream.headerSent {
		t.Fatal("RunQuery did not establish response headers")
	}
	if len(stream.responses) != 1 || stream.responses[0].GetDocument() != nil ||
		stream.responses[0].GetReadTime() == nil || !stream.responses[0].GetReadTime().IsValid() {
		t.Fatalf("empty query response = %+v", stream.responses)
	}
}

func TestRunQueryRejectsUnobservedShape(t *testing.T) {
	request := capturedWonderCourseQuery()
	request.GetStructuredQuery().From[0].CollectionId = "unknown"
	stream := &fakeUgcQueryStream{ctx: ugcQueryContext("u-test")}
	if err := (&ugcstoreServer{}).RunQuery(request, stream); status.Code(err) != codes.Unimplemented {
		t.Fatalf("status = %s, want Unimplemented", status.Code(err))
	}
}

func TestRunQueryRejectsMismatchedUIDMetadata(t *testing.T) {
	accessToken := mintNplnAccessToken(1800000001, nplnTenant+"/users/u-test", nplnTenant)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer "+accessToken,
		"npln-tenant-id", nplnTenantID,
		"uid", "u-other",
	))
	stream := &fakeUgcQueryStream{ctx: ctx}
	if err := (&ugcstoreServer{}).RunQuery(capturedWonderCourseQuery(), stream); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("status = %s, want PermissionDenied", status.Code(err))
	}
}
