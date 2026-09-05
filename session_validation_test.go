package main

import (
	"bytes"
	"encoding/base64"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	commonpb "npln.nintendo.net/npln-practice/proto/common"
	gspb "npln.nintendo.net/npln-practice/proto/gamesync/v1"
)

// Exact protobuf request from capture wonder-daemon-20260904-213039,
// payload 000870 (rpc-000156). Contains no authentication token.
func TestCapturedSignallingAllowsAbsentMetadata(t *testing.T) {
	wire, err := base64.StdEncoding.DecodeString("CpcCCpQCCowCCiNkb2NzL19fc3R1L3VzLTE3ODg1NzkyNTM2OTU1NTA4MDAtMhLkAQp/CgJwbBJ5QncBEQBYAAAAAuIVAAAAAGtJ0gGo+OOXkdSnwAEABAAAAAAA//////////8AAAAAAABOIU6WAAEAAP//////////AAAAAAAATiJOlgD/AAAAAAAAAAAAAAAAAAAAAAAAAAAA/wAAAAAAAAAAAAAAAAAAAAAAAAAAAQogCgRzdWlkEhg6FnUtbTd0anMyaGlkbDRoaHAycmxnZmEKDAoGc3VzY2lkEgIYAQojCgVzdXNpZBIaOhh1cy0xNzg4NTc5MjM0NTE1MTg2MDAwLTEKDAoGc3Vzc2lkEgIYARIDCgEq")
	if err != nil {
		t.Fatal(err)
	}
	request := &gspb.WriteDocumentsRequest{}
	if err := proto.Unmarshal(wire, request); err != nil {
		t.Fatal(err)
	}
	doc := request.WriteOperations[0].GetUpdateDocument().Document
	if _, present := doc.Fields.Fields["mp"]; present {
		t.Fatal("fixture unexpectedly contains mp")
	}
	if len(doc.Fields.Fields) != 5 {
		t.Fatal("fixture field count changed")
	}
	r := testRegistry()
	m := newMatchmaker(r)
	g := newGamesyncServer(r)
	host := testGamesyncJoin(t, g, testMatched(t, m, "u-host", "CourseMatch_20221202", "signal-pair"), "u-host")
	guest := testGamesyncJoin(t, g, testMatched(t, m, "u-guest", "CourseMatch_20221202", "signal-pair"), "u-guest")
	// Rebind only captured identities to the isolated test session. Preserve
	// opaque signalling bytes, omitted metadata and wildcard update mask.
	doc.Name = "docs/__stu/" + lastResourceSegment(guest.UserSession)
	doc.Fields.Fields["suid"] = gamesyncStringValue(host.UID)
	doc.Fields.Fields["susid"] = gamesyncStringValue(lastResourceSegment(host.UserSession))
	doc.Fields.Fields["sussid"] = gamesyncIntegerValue(host.Sequence)
	doc.Fields.Fields["suscid"] = gamesyncIntegerValue(host.Connection)
	wantPayload := append([]byte(nil), doc.Fields.Fields["pl"].GetBytesValue()...)
	if _, err := g.WriteDocuments(gamesyncContextForSession(host), request); err != nil {
		t.Fatalf("captured envelope rejected: %v", err)
	}
	got, err := g.GetDocument(gamesyncContextForSession(guest), &gspb.GetDocumentRequest{Name: doc.Name})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantPayload, got.Fields.Fields["pl"].GetBytesValue()) {
		t.Fatal("opaque payload changed")
	}
	if _, present := got.Fields.Fields["mp"]; present {
		t.Fatal("server fabricated metadata")
	}
	// Optional does not mean untyped: map is valid; scalar/null are invalid.
	doc.Fields.Fields["mp"] = &commonpb.Value{ValueType: &commonpb.Value_MapValue{MapValue: cloneFields(nil)}}
	if _, err := g.WriteDocuments(gamesyncContextForSession(host), request); err != nil {
		t.Fatalf("map rejected: %v", err)
	}
	for _, bad := range []*commonpb.Value{gamesyncStringValue("not a map"), nil} {
		doc.Fields.Fields["mp"] = bad
		if _, err := g.WriteDocuments(gamesyncContextForSession(host), request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid metadata accepted: %v", err)
		}
	}
}

func TestSessionResourceNameRejectsNestedAndEmptyIDs(t *testing.T) {
	for _, prefix := range []string{nplnTenant, "tenants/current"} {
		if !validSessionName(prefix + "/gameSessions/gs-test") {
			t.Fatal("valid session rejected")
		}
		for _, suffix := range []string{"", ".", "..", "gs-test/userSessions/other"} {
			if validSessionName(prefix + "/gameSessions/" + suffix) {
				t.Fatalf("invalid session accepted: %q", suffix)
			}
		}
	}
}

func TestSignallingConnectionIDRequiresRecoveredIntegerType(t *testing.T) {
	r := testRegistry()
	m := newMatchmaker(r)
	g := newGamesyncServer(r)
	s := testGamesyncJoin(t, g, testMatched(t, m, "u-host", "CourseMatch_20221202", "signal"), "u-host")
	fields := &commonpb.MapValue{Fields: map[string]*commonpb.Value{
		"suid":   gamesyncStringValue(s.UID),
		"susid":  gamesyncStringValue(lastResourceSegment(s.UserSession)),
		"sussid": gamesyncIntegerValue(s.Sequence),
		"suscid": gamesyncStringValue("1"),
		"pl":     {ValueType: &commonpb.Value_BytesValue{BytesValue: []byte{1}}},
		"mp":     {ValueType: &commonpb.Value_MapValue{MapValue: cloneFields(nil)}},
	}}
	if err := writableDocument("docs/__stg/All", fields, s, nil, nil, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("string connection ID accepted: %v", err)
	}
	fields.Fields["suscid"] = gamesyncIntegerValue(s.Connection)
	if err := writableDocument("docs/__stg/All", fields, s, nil, nil, nil); err != nil {
		t.Fatalf("typed signalling envelope rejected: %v", err)
	}
	fields.Fields["suid"] = gamesyncStringValue("u-other")
	if err := writableDocument("docs/__stg/All", fields, s, nil, nil, nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged sender accepted: %v", err)
	}
}
