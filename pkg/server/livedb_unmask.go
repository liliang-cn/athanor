package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
)

// Turning a token back into what it stood for.
//
// This is the one route in Athanor whose successful response is personal data.
// Everything else about the live-database path exists to keep such data out of
// the graph; this is the door that was left, on purpose, because a masked
// value that can never be recovered is not masked, it is destroyed — and an
// operator who redacts an engineer's address out of a knowledge graph still
// has to be able to page that engineer.
//
// So it is built as the exception it is:
//
//   - It has its OWN operation name. A key policy can grant "athanor.livedb"
//     — propose, sign, import — without granting "athanor.livedb.unmask". The
//     person who builds the graph and the person who may read a name out of it
//     are not necessarily the same person, and a single permission covering
//     both would make them one.
//   - Every call is a ledger entry, whether or not it found anything. The
//     entry names the caller, the count, and the tokens asked for; it never
//     names a value. "Who un-masked what, and when" is the record this feature
//     is worth having, and a reveal that leaves no trace is worse than no
//     reveal.
//   - A token that does not resolve is simply absent from the answer. It is
//     not an error and not a distinguishable "no such token", because a route
//     that answers differently for "exists but not yours" and "does not exist"
//     is an oracle for what the vault holds.
const livedbUnmaskOperation = "athanor.livedb.unmask"

// livedbUnmaskRequest is the body of POST /athanor/livedb/unmask.
type livedbUnmaskRequest struct {
	// Tokens are the values to resolve, as they appear in the graph.
	Tokens []string `json:"tokens"`
	// Why is the reason, which is recorded. It is required: a reveal nobody
	// had to justify is a reveal nobody will remember the reason for.
	Why string `json:"why"`
}

// livedbUnmaskLimit bounds one request. A reveal is meant to be a person
// looking something up, not a way to walk the vault back into a spreadsheet.
const livedbUnmaskLimit = 64

func (s *Server) handleLivedbUnmask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST a {tokens, why} document")
		return
	}
	key, code, msg := s.httpKey(r.Header.Get("Authorization"))
	if code != 0 {
		httpError(w, code, msg)
		return
	}
	// A reveal is a write in the sense that matters: it is an act that gets
	// recorded, and read-only is exactly the clearance a person auditing the
	// graph holds. They may read the graph; they may not un-mask it.
	if err := key.AuthorizeOperation(livedbUnmaskOperation, authz.Method{Access: authz.Write}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return
	}

	var req livedbUnmaskRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Why) == "" {
		httpError(w, http.StatusBadRequest, "why is required: a reveal is recorded, and a record with no reason is not one")
		return
	}
	tokens := dedupeTokens(req.Tokens)
	if len(tokens) == 0 {
		httpError(w, http.StatusBadRequest, "tokens is required")
		return
	}
	if len(tokens) > livedbUnmaskLimit {
		httpError(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d tokens per request: a reveal is a lookup, not an export", livedbUnmaskLimit))
		return
	}

	vault, provider, tenant, err := s.vault()
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if vault == nil {
		httpError(w, http.StatusNotImplemented, errNoVaultConfigured.Error())
		return
	}

	values, err := connector.Unmask(r.Context(), vault, tenant, tokens, provider)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the vault could not be read")
		return
	}

	// Recorded before the answer is written, and the answer is written whether
	// or not the record was: a reveal that happened is a fact, and a ledger
	// that could not be written is the ledger's problem to report.
	s.recordUnmask(r, key, tokens, len(values), req.Why)

	writeJSON(w, http.StatusOK, map[string]any{
		"values": values,
		// Said out loud so a caller does not read an absent token as an error.
		"unresolved": len(tokens) - len(values),
	})
}

// recordUnmask puts the reveal on the ledger. The tokens are named; no value
// ever is, which is what lets the ledger itself be read by somebody who may
// not un-mask.
func (s *Server) recordUnmask(r *http.Request, key authz.Key, tokens []string, found int, why string) {
	entry := ledgerEntry{
		ID:      mintUnmaskID(),
		Kind:    livedbKindUnmask,
		Actor:   key.ID,
		Verdict: "revealed",
		Note:    fmt.Sprintf("un-masked %d of %d token(s): %s", found, len(tokens), why),
		Detail: map[string]any{
			"tokens": tokens,
			"asked":  len(tokens),
			"found":  found,
			"why":    why,
		},
	}
	if _, err := s.ledger.record(r.Context(), entry); err != nil {
		log.Printf("athanor: ledger: un-mask by %s: %v", key.ID, err)
	}
}

// livedbKindUnmask groups reveals with the rest of pkg/livedb's acts.
const livedbKindUnmask = "livedb.unmask"

// mintUnmaskID gives every reveal its own entry.
//
// Deliberately not deterministic in what was asked for. Every other entry this
// server writes is keyed on the thing it is about, so that re-recording an act
// converges on one entry — which is right for a plan, whose proposal and
// signature are the same plan. A reveal is not like that: the same person
// looking up the same token twice is two events, and an id that collapsed them
// would erase the second from the only record there is of it.
func mintUnmaskID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A ledger id that cannot be minted still has to be an id. The clock
		// is a poor unique key and a good enough one for a fallback that
		// should never run.
		return "athanor:livedb:unmask:" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "athanor:livedb:unmask:" + hex.EncodeToString(b[:])
}

func dedupeTokens(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if t = strings.TrimSpace(t); t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
