package server

import "github.com/liliang-cn/cortexdb/v2/pkg/authz"

// alchemyPrefix is every RPC the pipeline service exposes.
const alchemyPrefix = "/alchemy.v1.Alchemy/"

// alchemyAccess classifies each pipeline RPC as a read or a write, for the
// same policy CortexDB applies to its own: a read-only key may look and may
// not change.
//
// It is an explicit table and not a rule about names, for the reason
// pkg/authz gives about its own: a HasPrefix("Delete") heuristic misclassifies
// the next RPC somebody adds, silently, in the direction that matters.
// TestEveryPipelineRPCIsClassified walks the registered service and fails on
// any method missing here, so a new RPC cannot arrive without a decision. An
// unclassified method is denied, not allowed.
//
// The three that read like queries and are writes: Decide stamps a person's
// name onto the graph; ExtendOntology publishes a new vocabulary version; and
// Assert adds a fact. CreateJob is a write because it spends the caller's
// model budget and admits work into a bounded store, and UploadSource because
// it puts bytes on the spool.
var alchemyAccess = map[string]authz.Access{
	alchemyPrefix + "UploadSource":   authz.Write,
	alchemyPrefix + "CreateJob":      authz.Write,
	alchemyPrefix + "GetJob":         authz.Read,
	alchemyPrefix + "WatchJob":       authz.Read,
	alchemyPrefix + "GetResult":      authz.Read,
	alchemyPrefix + "StreamResult":   authz.Read,
	alchemyPrefix + "DeleteJob":      authz.Write,
	alchemyPrefix + "Review":         authz.Read,
	alchemyPrefix + "ListFindings":   authz.Read,
	alchemyPrefix + "Decide":         authz.Write,
	alchemyPrefix + "ExtendOntology": authz.Write,
	alchemyPrefix + "Assert":         authz.Write,
}
