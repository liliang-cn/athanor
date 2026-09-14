package server

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	"github.com/liliang-cn/alchemy/pkg/wire"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
)

// Reading a graph that does not fit in one message.
//
// GetResult refuses a result over the limit rather than truncating it, and
// names StreamResult in the refusal. Athanor called GetResult and passed the
// refusal on, with a sentence admitting the gap — "StreamResult is not wired
// into loads yet" — and that is where a real corpus stopped: sixty-seven
// documents extracted for twenty-nine minutes, twenty-eight conflicts answered
// by a person, and then a 3.05MB graph that could not be put into the brain at
// all. The pipeline's most expensive step succeeded and its cheapest failed.
//
// Now there is one path and it is this one. StreamResult on a small graph is a
// single page, so nothing needs to choose between two readers, and both RPCs
// go through the same finished() check — a held job is refused identically,
// which is the property that must not be lost by changing which one is called.

// pageSink collects a server stream in memory.
//
// The pages are buffered rather than fed to the loader as they arrive, and
// that is a deliberate limit rather than an oversight: sink.Load takes a whole
// Result, and a graph is refused or loaded as one thing (§4.1's digest is over
// all of it). What this removes is the message limit, not the memory one — a
// graph far larger than this process should hold is still a graph this process
// should not be asked to load, and the honest place to say so is a limit
// somebody chooses, not a 3MB frame nobody meant as one.
type pageSink struct {
	// ServerStream is embedded to satisfy the interface and is deliberately
	// nil: StreamResult calls Context and Send and nothing else, and a nil
	// embedded interface turns any other call into a panic in a test rather
	// than a silent no-op in production.
	grpc.ServerStream
	ctx   context.Context
	pages []*alchemyv1.ResultPage
}

func (p *pageSink) Context() context.Context { return p.ctx }

func (p *pageSink) Send(page *alchemyv1.ResultPage) error {
	// Cloned: the server is free to reuse the message it sent, and a consumer
	// holding the pointer would end up with the last page repeated N times.
	p.pages = append(p.pages, page)
	return nil
}

func (p *pageSink) SetHeader(metadata.MD) error  { return nil }
func (p *pageSink) SendHeader(metadata.MD) error { return nil }
func (p *pageSink) SetTrailer(metadata.MD)       {}

// resultOf reads a finished job's graph, however many messages it takes.
func (s *Server) resultOf(ctx context.Context, job string) (alchemy.Result, error) {
	sink := &pageSink{ctx: ctx}
	if err := s.alchemy.StreamResult(&alchemyv1.GetResultRequest{JobId: job}, sink); err != nil {
		return alchemy.Result{}, err
	}
	return wire.Pages(sink.pages)
}
