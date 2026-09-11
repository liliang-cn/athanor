package server

import "net/http"

// Placeholders for the two screens being written alongside this one. Each is
// replaced wholesale by the file that owns it — ui_ledger.go and
// ui_import.go — and this file is deleted with the second of them. It exists
// so that the routes can be registered once, in server.go, and neither author
// has to touch that file.

func (s *Server) handleUIDecisions(w http.ResponseWriter, r *http.Request) {
	s.uiNotYet(w, r, "Decisions", "/app/decisions")
}

func (s *Server) handleUIDecisionChain(w http.ResponseWriter, r *http.Request) {
	s.uiNotYet(w, r, "Decision", "/app/decisions")
}

func (s *Server) handleUIImport(w http.ResponseWriter, r *http.Request) {
	s.uiNotYet(w, r, "Import", "/app/import")
}

func (s *Server) handleUIImportLiveDB(w http.ResponseWriter, r *http.Request) {
	s.uiNotYet(w, r, "Import from a live database", "/app/import")
}

func (s *Server) handleUIImportRuns(w http.ResponseWriter, r *http.Request) {
	s.uiNotYet(w, r, "Runs", "/app/import")
}

func (s *Server) handleUIImportFollows(w http.ResponseWriter, r *http.Request) {
	s.uiNotYet(w, r, "Follows", "/app/import")
}

func (s *Server) uiNotYet(w http.ResponseWriter, r *http.Request, title, nav string) {
	if _, ok := s.uiKey(w, r); !ok {
		return
	}
	s.renderUI(w, uiPage{
		Title: title, Nav: nav,
		Lede:   "This screen is not built yet. The act it will perform is already an API.",
		Notice: "Not built yet.",
		Body:   "",
	})
}
