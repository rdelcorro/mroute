package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/mroute/internal/flow"
	"github.com/openclaw/mroute/pkg/types"
)

type Server struct {
	mgr    *flow.Manager
	mux    *http.ServeMux
	server *http.Server
}

func NewServer(addr string, mgr *flow.Manager) *Server {
	s := &Server{mgr: mgr, mux: http.NewServeMux()}
	s.routes()
	s.server = &http.Server{
		Addr:         addr,
		Handler:      s.logging(s.mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.health)
	s.mux.HandleFunc("/v1/flows", s.handleFlows)
	s.mux.HandleFunc("/v1/flows/", s.handleFlowDetail)
	s.mux.HandleFunc("/v1/events", s.handleEvents)
}

func (s *Server) Start() error {
	log.Printf("API server on %s", s.server.Addr)
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(t))
	})
}

// --- Flows ---

func (s *Server) handleFlows(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		flows, err := s.mgr.ListFlows()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"flows": flows})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
		var f types.Flow
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			writeErr(w, 400, "invalid body")
			return
		}
		if err := s.mgr.CreateFlow(&f); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, f)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) handleFlowDetail(w http.ResponseWriter, r *http.Request) {
	// Parse: /v1/flows/{id}[/{action}[/{param}]]
	path := strings.TrimPrefix(r.URL.Path, "/v1/flows/")
	parts := strings.SplitN(path, "/", 3)
	id := parts[0]
	action := ""
	param := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if len(parts) > 2 {
		param = parts[2]
	}

	if id == "" {
		writeErr(w, 400, "flow id required")
		return
	}

	switch action {
	case "":
		s.flowCRUD(w, r, id)
	case "start":
		s.flowStart(w, r, id)
	case "stop":
		s.flowStop(w, r, id)
	case "source":
		s.flowSource(w, r, id, param)
	case "outputs":
		s.flowOutputs(w, r, id, param)
	case "metrics":
		s.flowMetrics(w, r, id)
	case "events":
		s.flowEvents(w, r, id)
	default:
		writeErr(w, 404, "unknown action: "+action)
	}
}

func (s *Server) flowCRUD(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		f, err := s.mgr.GetFlow(id)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, f)
	case http.MethodDelete:
		if err := s.mgr.DeleteFlow(id); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		w.WriteHeader(204)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) flowStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "use POST")
		return
	}
	if err := s.mgr.StartFlow(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	f, err := s.mgr.GetFlow(id)
	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"id": id, "message": "flow started"})
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"id":      id,
		"status":  f.Status,
		"message": "flow started",
	})
}

func (s *Server) flowStop(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "use POST")
		return
	}
	if err := s.mgr.StopFlow(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	f, err := s.mgr.GetFlow(id)
	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"id": id, "message": "flow stopped"})
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"id":      id,
		"status":  f.Status,
		"message": "flow stopped",
	})
}

func (s *Server) flowSource(w http.ResponseWriter, r *http.Request, id, sourceName string) {
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var src types.Source
		if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
			writeErr(w, 400, "invalid body")
			return
		}
		if err := s.mgr.AddSource(id, &src); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.writeFlow(w, id)
	case http.MethodDelete:
		if sourceName == "" {
			writeErr(w, 400, "source name required")
			return
		}
		if err := s.mgr.RemoveSource(id, sourceName); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.writeFlow(w, id)
	default:
		writeErr(w, 405, "use POST or DELETE")
	}
}

func (s *Server) flowOutputs(w http.ResponseWriter, r *http.Request, id, outputName string) {
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var out types.Output
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			writeErr(w, 400, "invalid body")
			return
		}
		if err := s.mgr.AddOutput(id, &out); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.writeFlow(w, id)
	case http.MethodPut:
		if outputName == "" {
			writeErr(w, 400, "output name required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var updates types.Output
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			writeErr(w, 400, "invalid body")
			return
		}
		if err := s.mgr.UpdateOutput(id, outputName, &updates); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.writeFlow(w, id)
	case http.MethodDelete:
		if outputName == "" {
			writeErr(w, 400, "output name required")
			return
		}
		if err := s.mgr.RemoveOutput(id, outputName); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.writeFlow(w, id)
	default:
		writeErr(w, 405, "use POST, PUT, or DELETE")
	}
}

func (s *Server) flowMetrics(w http.ResponseWriter, r *http.Request, id string) {
	m := s.mgr.GetMetrics(id)
	if m == nil {
		writeErr(w, 404, "no metrics (flow not active)")
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) flowEvents(w http.ResponseWriter, r *http.Request, id string) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 10000 {
		limit = 10000
	}
	events, err := s.mgr.GetEvents(id, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"events": events})
}

// --- Events (global) ---

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 10000 {
		limit = 10000
	}
	events, err := s.mgr.GetEvents("", limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"events": events})
}

// --- Health ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	total, active, err := s.mgr.FlowCounts()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"status":       "ok",
		"total_flows":  total,
		"active_flows": active,
	})
}

func (s *Server) writeFlow(w http.ResponseWriter, id string) {
	f, err := s.mgr.GetFlow(id)
	if err != nil {
		writeErr(w, 500, "failed to retrieve flow: "+err.Error())
		return
	}
	writeJSON(w, 200, f)
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
