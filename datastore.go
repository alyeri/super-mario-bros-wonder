package main

// datastore — partial in-memory nn.npln.hydro.v1.Datastore implementation.
// The meaning of Wonder payloads is not assumed until captured traffic proves it.

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	hydropb "npln.nintendo.net/npln-practice/proto/hydro/v1"
)

type datastoreServer struct {
	hydropb.UnimplementedDatastoreServer
	mu       sync.RWMutex
	contents map[string]*hydropb.Content
}

func newDatastoreServer() *datastoreServer {
	return &datastoreServer{
		contents: make(map[string]*hydropb.Content),
	}
}

func (d *datastoreServer) CreateContent(ctx context.Context, req *hydropb.CreateContentRequest) (*hydropb.Content, error) {
	c := req.GetContent()
	if c == nil {
		c = &hydropb.Content{}
	}
	contentID := fmt.Sprintf("content-%d", time.Now().UnixNano())
	c.Name = req.GetParent() + "/contents/" + contentID
	c.CreateTime = timestamppb.Now()
	c.UpdateTime = timestamppb.Now()

	d.mu.Lock()
	d.contents[c.Name] = c
	d.mu.Unlock()

	log.Printf("[NPLN Datastore] CreateContent parent=%q name=%q (payload size: %d bytes)",
		req.GetParent(), c.Name, len(c.GetPayload()))
	return c, nil
}

func (d *datastoreServer) GetContent(ctx context.Context, req *hydropb.GetContentRequest) (*hydropb.Content, error) {
	d.mu.RLock()
	c, ok := d.contents[req.GetName()]
	d.mu.RUnlock()

	if !ok {
		log.Printf("[NPLN Datastore] GetContent name=%q (not found)", req.GetName())
		return &hydropb.Content{
			Name:       req.GetName(),
			CreateTime: timestamppb.Now(),
			UpdateTime: timestamppb.Now(),
		}, nil
	}
	log.Printf("[NPLN Datastore] GetContent name=%q found", req.GetName())
	return c, nil
}

func (d *datastoreServer) SearchContent(ctx context.Context, req *hydropb.SearchContentRequest) (*hydropb.SearchContentResponse, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var results []*hydropb.Content
	for _, c := range d.contents {
		results = append(results, c)
		if len(results) >= 50 {
			break
		}
	}
	log.Printf("[NPLN Datastore] SearchContent tenant=%q type=%q -> %d results",
		req.GetTenant(), req.GetSearchType(), len(results))
	return &hydropb.SearchContentResponse{Contents: results}, nil
}

func (d *datastoreServer) DeleteContent(ctx context.Context, req *hydropb.DeleteContentRequest) (*emptypb.Empty, error) {
	d.mu.Lock()
	delete(d.contents, req.GetName())
	d.mu.Unlock()

	log.Printf("[NPLN Datastore] DeleteContent name=%q", req.GetName())
	return &emptypb.Empty{}, nil
}
