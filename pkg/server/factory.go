package server

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	"github.com/liliang-cn/alchemy/pkg/model"
	"github.com/liliang-cn/alchemy/pkg/runner"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// modelFactory makes a job's endpoints real, with alchemy's own HTTP client.
// Models are supplied per job (alchemy DESIGN.md §6): nothing here hardcodes a
// host, a key or a model name.
type modelFactory struct{}

func (modelFactory) LLM(e runner.Endpoint) (alchemy.LLM, error) { return model.NewLLM(endpoint(e)) }
func (modelFactory) Embedder(e runner.Endpoint) (alchemy.Embedder, error) {
	return model.NewEmbedder(endpoint(e))
}
func (modelFactory) OCR(e runner.Endpoint) (alchemy.OCR, error) { return model.NewOCR(endpoint(e)) }

func endpoint(e runner.Endpoint) model.Endpoint {
	return model.Endpoint{Name: e.Name, BaseURL: e.BaseURL, APIKey: e.APIKey, Options: e.Options}
}

// brainEmbedder adapts alchemy's batch-only Embedder to the shape CortexDB's
// recall wants, so one HTTP client serves both sides of the process. The
// dimension is declared rather than probed: CortexDB locks a store to the
// width of its first vector, and a wrong number here is a store that can never
// search part of itself. It is the operator's to state.
type brainEmbedder struct {
	inner alchemy.Embedder
	dim   int
}

func (b brainEmbedder) Dim() int { return b.dim }

func (b brainEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := b.inner.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(out) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors for one text", len(out))
	}
	return out[0], nil
}

func (b brainEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	return b.inner.Embed(ctx, texts)
}

// EmbedderFromEnv builds the brain's embedder from the variables cortexdb-grpc
// already documents, so a deployment written for one works for the other:
//
//	OPENAI_BASE_URL       enables it; unset is lexical mode
//	OPENAI_API_KEY        optional
//	CORTEXDB_EMBED_MODEL  default text-embedding-3-small
//	CORTEXDB_EMBED_DIM    default 1536
func EmbedderFromEnv() (cortexdb.Embedder, string, error) {
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		return nil, "", nil
	}
	name := os.Getenv("CORTEXDB_EMBED_MODEL")
	if name == "" {
		name = "text-embedding-3-small"
	}
	dim := 1536
	if raw := os.Getenv("CORTEXDB_EMBED_DIM"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, "", fmt.Errorf("CORTEXDB_EMBED_DIM %q is not a positive integer", raw)
		}
		dim = n
	}
	inner, err := model.NewEmbedder(model.Endpoint{Name: name, BaseURL: base, APIKey: os.Getenv("OPENAI_API_KEY")})
	if err != nil {
		return nil, "", err
	}
	return brainEmbedder{inner: inner, dim: dim}, fmt.Sprintf("%s (dim=%d) via %s", name, dim, base), nil
}
