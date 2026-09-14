package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	alchemycdb "github.com/liliang-cn/alchemy/connectors/cortexdb"
)

// A vocabulary this brain could never load under is refused when it is written.
//
// The store keeps a few property names for itself — name, description, type,
// and the handful that hold a record's provenance — and an attribute landing on
// one of them would not collide loudly, it would quietly change what the store
// thinks the record is. The connector refuses that, and refuses it at the last
// possible moment: a graph is extracted, its queue is answered by a person, and
// the load then fails on the first entity.
//
// On sixty-seven documents that was thirty-one minutes of model calls and
// twenty-eight questions for a person, all spent under a vocabulary that could
// not have worked from the moment it was written. Every input to that judgement
// was on hand before the first call: the declared attributes are in the
// document, and the reserved names are a constant.
//
// It cannot be checked in alchemy, which holds no store and so has no opinion
// about any store's property names, and it cannot be checked in the connector,
// which never sees an ontology. Athanor holds both. This is the whole of what
// holding both is for.

// reservedAttributes reports the declared attributes this brain would refuse,
// as sentences a person can act on.
func reservedAttributes(document []byte) []string {
	var doc struct {
		Parts map[string]struct {
			Entities []struct {
				Name       string   `json:"name"`
				Attributes []string `json:"attributes"`
			} `json:"entities"`
			Relations []struct {
				Name       string   `json:"name"`
				Attributes []string `json:"attributes"`
			} `json:"relations"`
		} `json:"parts"`
	}
	// A document this cannot read is not refused here. It is about to be
	// parsed properly by the ontology store, which owns that error and writes
	// it better than this could.
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil
	}

	nodes := set(alchemycdb.ReservedNodeProperties())
	edges := set(alchemycdb.ReservedEdgeProperties())
	var bad []string
	for partName, part := range doc.Parts {
		where := func(kind, typeName string) string {
			if partName == "" || partName == "prose" {
				return fmt.Sprintf("%s %q", kind, typeName)
			}
			return fmt.Sprintf("%s %q of part %q", kind, typeName, partName)
		}
		for _, e := range part.Entities {
			for _, a := range e.Attributes {
				if _, clash := nodes[a]; clash {
					bad = append(bad, fmt.Sprintf(
						"%s declares the attribute %q, which this brain writes on every entity node itself",
						where("entity type", e.Name), a))
				}
			}
		}
		for _, rel := range part.Relations {
			for _, a := range rel.Attributes {
				if _, clash := edges[a]; clash {
					bad = append(bad, fmt.Sprintf(
						"%s declares the attribute %q, which this brain writes on every relation edge itself",
						where("relation type", rel.Name), a))
				}
			}
		}
	}
	sort.Strings(bad)
	return bad
}

// reservedRefusal is the sentence the route answers with, naming every clash
// and what the brain keeps, so that one round trip is enough to fix the
// document.
func reservedRefusal(bad []string) string {
	return fmt.Sprintf(
		"this brain cannot load a graph under that vocabulary: %s. It keeps %s on a node and %s on an edge; rename the attribute, and the source's own word for it stays in the record's text either way",
		strings.Join(bad, "; "),
		quoteAll(alchemycdb.ReservedNodeProperties()),
		quoteAll(alchemycdb.ReservedEdgeProperties()))
}

func set(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

func quoteAll(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, `"`+n+`"`)
	}
	return strings.Join(out, ", ")
}
