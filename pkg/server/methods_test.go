package server

import (
	"testing"

	"google.golang.org/grpc"

	"github.com/liliang-cn/alchemy/pkg/service"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
)

// The pipeline's RPCs are classified by a table, and this is what keeps the
// table honest: every method alchemy registers must be in it, and everything
// in it must still be registered. A new RPC upstream cannot arrive here
// without a decision — the merge fails naming it.
func TestEveryPipelineRPCIsClassifiedAsAReadOrAWrite(t *testing.T) {
	gs := grpc.NewServer()
	alchemyv1.RegisterAlchemyServer(gs, &service.Server{})
	info, ok := gs.GetServiceInfo()["alchemy.v1.Alchemy"]
	if !ok || len(info.Methods) == 0 {
		t.Fatal("the pipeline service registered no methods; this test would pass vacuously")
	}
	for _, m := range info.Methods {
		full := alchemyPrefix + m.Name
		if _, classified := alchemyAccess[full]; !classified {
			t.Errorf("%s is served but not classified; add it to alchemyAccess in methods.go as a read or a write — an unclassified RPC is denied to every key", full)
		}
	}
	served := map[string]bool{}
	for _, m := range info.Methods {
		served[alchemyPrefix+m.Name] = true
	}
	for full := range alchemyAccess {
		if !served[full] {
			t.Errorf("%s is classified but no longer served; remove it from alchemyAccess", full)
		}
	}
}
