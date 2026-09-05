package server

import (
	"context"
	"fmt"
	"log"
	"strings"

	"google.golang.org/grpc"

	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// A review decision becomes a ledger entry, from inside the chain.
//
// The act is alchemy's — Decide applies a person's judgement to a finding and
// unblocks a held job — and Athanor does not reimplement it to record it. It
// observes it, from an interceptor placed **after** authorization, so that
// nothing unauthorized is ever recorded and the actor is the key the policy
// resolved rather than anything in the request.
//
// Two hooks because there are two doors to the same act. Decide is a unary
// batch — the shape a job sitting at NEEDS_REVIEW actually has — and Review is
// the bidirectional stream, which exists because a decision can reach an
// extraction that has not run yet. The unary hook is exact: DecideResponse
// names the decisions the job did not recognise, so those are not recorded.
// The stream hook is coarser and says so below.

// callerKeyContext is the private type the authorization interceptor parks the
// resolved key under. Unexported, and a distinct type rather than a string, so
// nothing outside this package can forge a caller identity by writing to a
// context — CortexDB's rule in pkg/rpcserver/ownership.go, for the same reason.
type callerKeyContext struct{}

// withCallerKey carries the authenticated key forward to the hooks below.
//
// It has to be carried rather than re-read: by the time a call reaches the
// handler, auth.go has replaced the caller's Authorization metadata with the
// process's own internal token, so the credential in the context is Athanor's
// and not the caller's. That swap is deliberate and stays; this is how the
// identity survives it.
func withCallerKey(ctx context.Context, key authz.Key) context.Context {
	return context.WithValue(ctx, callerKeyContext{}, key)
}

// callerKey reports who is calling, if authorization resolved anybody. With no
// key policy at all there is no key, and the entries recorded then are signed
// with the open-deployment id httpKey already uses for the same situation.
func callerKey(ctx context.Context) authz.Key {
	if key, ok := ctx.Value(callerKeyContext{}).(authz.Key); ok && key.ID != "" {
		return key
	}
	return authz.Key{ID: openKeyID}
}

// openKeyID is the actor an entry carries when there is no key policy. It is
// not a name anybody could hold: an operator reading the ledger of an open
// deployment should see that the door was open, not a plausible person.
const openKeyID = "open"

const (
	methodDecide = alchemyPrefix + "Decide"
	methodReview = alchemyPrefix + "Review"
)

// ledgerUnary records a Decide that succeeded.
func (s *Server) ledgerUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod != methodDecide {
			return handler(ctx, req)
		}
		decide, ok := req.(*alchemyv1.DecideRequest)
		if !ok {
			return handler(ctx, req)
		}
		// The findings are read before the batch is applied, because applying
		// it is what takes them off the queue. What they carry that a
		// ReviewDecision does not is the subject and the shape — which is the
		// half of the entry a reader needs and the reviewer never typed.
		items := s.findingsByID(ctx, decide.GetJobId())

		resp, err := handler(ctx, req)
		if err != nil {
			return resp, err
		}
		refused := map[string]string{}
		if out, ok := resp.(*alchemyv1.DecideResponse); ok {
			for _, r := range out.GetRejected() {
				refused[r.GetItemId()] = r.GetReason()
			}
		}
		actor := callerKey(ctx).ID
		for _, d := range decide.GetDecisions() {
			if reason, rejected := refused[d.GetItemId()]; rejected {
				// alchemy did not apply it, so nothing was decided. The reason
				// is the caller's to see in the response; the ledger records
				// what happened, and this did not.
				_ = reason
				continue
			}
			s.recordReview(ctx, actor, decide.GetJobId(), d, items[d.GetItemId()])
		}
		return resp, nil
	}
}

// ledgerStream records the decisions a Review stream carried, once it closes
// without an error.
//
// This is coarser than the unary hook and the difference is worth naming: the
// stream acknowledges nothing per decision — a reviewer answers item three
// while item four is still arriving, which is the property the stream exists
// for — so there is no per-item verdict to read. What is recorded is therefore
// the decisions the reviewer submitted on a stream that completed, and a
// decision the service quietly ignored would be recorded as made. The
// alternative is recording nothing from the stream at all, which would leave
// the one door that can review a *running* job absent from the audit.
func (s *Server) ledgerStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if info.FullMethod != methodReview {
			return handler(srv, ss)
		}
		seen := &reviewWatcher{ServerStream: ss}
		if err := handler(srv, seen); err != nil {
			return err
		}
		ctx := ss.Context()
		actor := callerKey(ctx).ID
		byJob := map[string]map[string]*alchemyv1.ReviewItem{}
		for _, d := range seen.decisions {
			job := d.GetJobId()
			if job == "" {
				job = seen.job
			}
			if _, known := byJob[job]; !known {
				// One lookup per job on the stream, not per decision. A job
				// still running has no finding list to read and comes back
				// empty, which is the honest shape: the entry then names the
				// item and not the subject.
				byJob[job] = s.findingsByID(ctx, job)
			}
			s.recordReview(ctx, actor, job, d, byJob[job][d.GetItemId()])
		}
		return nil
	}
}

// reviewWatcher keeps the decisions a Review stream carried in.
//
// Only what the client sent: the items the server sends back are the queue,
// not the judgement, and a ledger of what somebody was shown is a different
// record from a ledger of what they decided.
type reviewWatcher struct {
	grpc.ServerStream
	job       string
	decisions []*alchemyv1.ReviewDecision
}

func (w *reviewWatcher) RecvMsg(m any) error {
	if err := w.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	d, ok := m.(*alchemyv1.ReviewDecision)
	if !ok {
		return nil
	}
	if w.job == "" && d.GetJobId() != "" {
		// The first message names the job; later ones may leave it empty.
		w.job = d.GetJobId()
	}
	if d.GetItemId() == "" {
		// A first message with a job and no item is how a reviewer attaches to
		// a queue without deciding anything. Attaching is not a decision.
		return nil
	}
	// Cloned, because the stream may reuse the message it decoded into.
	w.decisions = append(w.decisions, &alchemyv1.ReviewDecision{
		JobId: d.GetJobId(), ItemId: d.GetItemId(), Verb: d.GetVerb(),
		By: d.GetBy(), Note: d.GetNote(), Edit: d.GetEdit(),
	})
	return nil
}

// findingsByID reads a job's queue, indexed by item id. An error is an empty
// map: the ledger entry is worth less without the subject and is still worth
// writing.
func (s *Server) findingsByID(ctx context.Context, job string) map[string]*alchemyv1.ReviewItem {
	out := map[string]*alchemyv1.ReviewItem{}
	if strings.TrimSpace(job) == "" {
		return out
	}
	list, err := s.alchemy.ListFindings(ctx, &alchemyv1.ListFindingsRequest{JobId: job})
	if err != nil {
		return out
	}
	for _, item := range list.GetItems() {
		out[item.GetId()] = item
	}
	return out
}

// recordReview writes one review decision into the ledger.
//
// Its premises are empty, and that is a gap rather than an omission. A
// premise must already exist in the brain, and the two claims a conflict is
// between do not: the job is not loaded until after review, by construction —
// that is what /athanor/loads refusing a held job means. They are recorded as
// the entry's structured detail instead, exactly as the subject is, so nothing
// about the finding is lost; what is lost is the graph edge from the decision
// to the records, and the load that lands later closes half of it by taking
// this entry as one of its own premises (loads.go).
func (s *Server) recordReview(ctx context.Context, actor, job string, d *alchemyv1.ReviewDecision, item *alchemyv1.ReviewItem) {
	verb := reviewVerb(d.GetVerb())
	subject := item.GetSubject()
	detail := map[string]any{
		"job":  job,
		"item": d.GetItemId(),
		"verb": verb,
	}
	if subject != "" {
		detail["subject"] = subject
	}
	if shape := item.GetShape(); shape != "" {
		detail["shape"] = shape
	}
	if summary := item.GetSummary(); summary != "" {
		detail["summary"] = summary
	}
	// The name the reviewer typed, kept beside the key that actually called.
	// See ledger.go on `_by` and `by`.
	if by := strings.TrimSpace(d.GetBy()); by != "" {
		detail["by"] = by
	}
	if note := strings.TrimSpace(d.GetNote()); note != "" {
		detail["note"] = note
	}
	if e := d.GetEdit(); e != nil {
		edit := map[string]string{}
		for k, v := range map[string]string{
			"type": e.GetType(), "name": e.GetName(),
			"from": e.GetFrom(), "to": e.GetTo(), "into": e.GetInto(),
		} {
			if v != "" {
				edit[k] = v
			}
		}
		if len(edit) > 0 {
			detail["edit"] = edit
		}
	}

	line := fmt.Sprintf("%s finding %s of job %s", verb, d.GetItemId(), job)
	if subject != "" {
		line += ", about " + subject
	}
	entry := ledgerEntry{
		ID:      reviewDecisionID(job, d.GetItemId()),
		Kind:    ledgerKindReview,
		Actor:   actor,
		Verdict: verb,
		Subject: subject,
		Note:    line,
		Detail:  detail,
	}
	if _, err := s.ledger.record(ctx, entry); err != nil {
		// The judgement was applied before this ran and stands whatever
		// happens here. A review that was refused because its record failed
		// would leave the job held for a reason nobody asked for.
		log.Printf("athanor: ledger: review %s of job %s: %v", d.GetItemId(), job, err)
	}
}

// reviewVerb is the enum in a word a person would write: REVIEW_VERB_ACCEPT
// is "accept". The zero value has no verb and is reported as itself rather
// than guessed at.
func reviewVerb(v alchemyv1.ReviewVerb) string {
	name := strings.TrimPrefix(v.String(), "REVIEW_VERB_")
	if name == "" || name == "UNSPECIFIED" {
		return "unspecified"
	}
	return strings.ToLower(name)
}
