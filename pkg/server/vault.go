package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
)

// The vault: where the originals of reversible treatments live.
//
// A masking plan may say pseudonymize, which is the one treatment that can be
// undone. The graph gets a token; the value the token stands for is encrypted
// under a per-tenant key and written to a SQLite file that is NOT the brain.
// Keeping them apart is the whole point — a backup of the brain, a copy handed
// to an analyst, a leak of the graph, none of them carry the value, and the
// only thing that turns a token back is this file plus the key.
//
// # There is no default key
//
// A key generated at startup and written beside the vault would open the vault
// for anybody who has the vault, which is a decoration rather than a control.
// So an operator supplies one or there is no vault at all, and a plan holding
// a reversible treatment is then refused at signing with its columns named.
// That refusal is the honest outcome: the alternative is silently downgrading
// somebody's pseudonymize to a mask, which changes what they signed.
//
// # Opening it is lazy and once
//
// The vault is opened the first time something needs it and kept, because
// OpenSQLiteVault creates the file and a server that opens one per request
// would leave a handle per request. Close shuts it.
type vaultHandle struct {
	once   sync.Once
	vault  *connector.SQLiteVault
	keys   connector.KeyProvider
	tenant string
	err    error
}

// vaultTenantDefault names this deployment's tokens in a shared vault file.
const vaultTenantDefault = "athanor"

// vault opens the vault, or reports that this deployment has none.
//
// A nil vault with a nil error is the ordinary "no key configured" answer, and
// every caller must handle it — it is not a failure, it is a deployment that
// does not do reversible masking.
func (s *Server) vault() (connector.Vault, connector.KeyProvider, string, error) {
	h := &s.vaultOnce
	h.once.Do(func() {
		keyFile := strings.TrimSpace(s.opts.VaultKeyFile)
		if keyFile == "" {
			return // no key, no vault, no error
		}
		provider := connector.FileKeyProvider{Path: keyFile}
		// Read the key now rather than at the first Put. A key file that is
		// missing or the wrong length is an operator's mistake, and the place
		// to learn about it is the first request that wanted a vault, not the
		// middle of an import.
		if _, err := provider.TenantKey(nil, vaultTenantOf(s.opts.VaultTenant)); err != nil { //nolint:staticcheck // FileKeyProvider ignores ctx
			h.err = fmt.Errorf("athanor: vault key %s: %w", keyFile, err)
			return
		}
		path := strings.TrimSpace(s.opts.VaultPath)
		if path == "" {
			path = defaultVaultPath(s.opts.DBPath)
		}
		v, err := connector.OpenSQLiteVault(path)
		if err != nil {
			h.err = fmt.Errorf("athanor: open vault %s: %w", path, err)
			return
		}
		h.vault, h.keys, h.tenant = v, provider, vaultTenantOf(s.opts.VaultTenant)
	})
	if h.err != nil {
		return nil, nil, "", h.err
	}
	if h.vault == nil {
		return nil, nil, "", nil
	}
	return h.vault, h.keys, h.tenant, nil
}

func vaultTenantOf(configured string) string {
	if t := strings.TrimSpace(configured); t != "" {
		return t
	}
	return vaultTenantDefault
}

// defaultVaultPath puts the vault beside the brain when the brain is a file,
// and refuses to guess when it is a DSN — a PostgreSQL deployment has no
// obvious directory, and a vault silently created in the working directory is
// a vault nobody backs up.
func defaultVaultPath(dbPath string) string {
	if isPostgresDSN(dbPath) || strings.TrimSpace(dbPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "vault.db")
}

func isPostgresDSN(p string) bool {
	return strings.HasPrefix(p, "postgres://") || strings.HasPrefix(p, "postgresql://")
}

// closeVault releases the handle, if one was ever opened.
func (s *Server) closeVault() error {
	if s.vaultOnce.vault == nil {
		return nil
	}
	return s.vaultOnce.vault.Close()
}

// errNoVaultConfigured is what the unmask route answers when this deployment
// has no key. It names what is missing and not whether any token exists.
var errNoVaultConfigured = errors.New("this Athanor has no vault: set a vault key file to enable reversible masking")
