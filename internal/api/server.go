package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/clickety-clacks/lachesis/internal/core"
	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type Server struct {
	service *core.Service
	mux     *http.ServeMux
}

func New(service *core.Service) *Server {
	s := &Server{service: service, mux: http.NewServeMux()}
	s.routes()
	return s
}
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) { write(w, http.StatusOK, s.service.Health()) })
	s.mux.HandleFunc("GET /api/v1/help", s.helpIndex)
	s.mux.HandleFunc("GET /api/v1/help/{topic}", s.helpTopic)
	s.mux.HandleFunc("GET /api/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]any{"accounts": s.service.List()})
	})
	s.mux.HandleFunc("POST /api/v1/accounts/adopt", s.adopt)
	s.mux.HandleFunc("POST /api/v1/accounts", s.onboard)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.job)
	s.mux.HandleFunc("POST /api/v1/accounts/{id}/verify", s.verify)
	s.mux.HandleFunc("POST /api/v1/accounts/{id}/re-onboard", s.reOnboard)
	s.mux.HandleFunc("POST /api/v1/accounts/{id}/refresh", s.refresh)
	s.mux.HandleFunc("DELETE /api/v1/accounts/{id}", s.delete)
	s.mux.HandleFunc("GET /api/v1/usage", s.aggregate)
	s.mux.HandleFunc("GET /api/v1/accounts/{id}/usage", s.usage)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, teach.New(teach.HelpTopicNotFound, "The API path does not exist.", "accounts", nil, map[string]any{"path": r.URL.Path}, []model.RemedyCall{{Method: "GET", Path: "/api/v1/help"}}))
	})
}

func (s *Server) helpIndex(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(topics))
	for n := range topics {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n, "summary": topics[n].Summary, "path": "/api/v1/help/" + n})
	}
	write(w, http.StatusOK, map[string]any{"topics": out})
}
func (s *Server) helpTopic(w http.ResponseWriter, r *http.Request) {
	t, ok := topics[r.PathValue("topic")]
	if !ok {
		fail(w, teach.New(teach.HelpTopicNotFound, "The help topic does not exist.", "accounts", nil, map[string]any{"topic": r.PathValue("topic")}, []model.RemedyCall{{Method: "GET", Path: "/api/v1/help"}}))
		return
	}
	if t.Prerequisites == nil {
		t.Prerequisites = []helpPrereq{}
	}
	write(w, http.StatusOK, t)
}

type sourceRequest struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Service string `json:"service,omitempty"`
	Account string `json:"account,omitempty"`
}
type adoptRequest struct {
	Provider model.Provider `json:"provider"`
	Label    string         `json:"label"`
	Source   sourceRequest  `json:"source"`
}

func (s *Server) adopt(w http.ResponseWriter, r *http.Request) {
	var in adoptRequest
	if d := decode(r, &in); d != nil {
		fail(w, d)
		return
	}
	binding, d := s.service.ResolveSource(in.Provider, in.Source.Kind, in.Source.Path, in.Source.Service, in.Source.Account)
	if d != nil {
		fail(w, d)
		return
	}
	account, d := s.service.Adopt(r.Context(), in.Provider, in.Label, binding)
	if d != nil {
		fail(w, d)
		return
	}
	write(w, http.StatusCreated, account)
}

type onboardRequest struct {
	Provider model.Provider `json:"provider"`
	Label    string         `json:"label"`
}

func (s *Server) onboard(w http.ResponseWriter, r *http.Request) {
	var in onboardRequest
	if d := decode(r, &in); d != nil {
		fail(w, d)
		return
	}
	job, d := s.service.Jobs().StartOnboard(in.Provider, in.Label)
	if d != nil {
		fail(w, d)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+job.ID)
	write(w, http.StatusAccepted, job)
}
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	job, d := s.service.Jobs().Get(r.PathValue("id"))
	if d != nil {
		fail(w, d)
		return
	}
	write(w, http.StatusOK, job)
}
func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	if d := emptyBody(r); d != nil {
		fail(w, d)
		return
	}
	a, d := s.service.Verify(r.Context(), r.PathValue("id"))
	if d != nil {
		fail(w, d)
		return
	}
	write(w, http.StatusOK, a)
}
func (s *Server) reOnboard(w http.ResponseWriter, r *http.Request) {
	if d := emptyBody(r); d != nil {
		fail(w, d)
		return
	}
	j, d := s.service.Jobs().StartReOnboard(r.PathValue("id"))
	if d != nil {
		fail(w, d)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	write(w, http.StatusAccepted, j)
}
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if d := emptyBody(r); d != nil {
		fail(w, d)
		return
	}
	a, d := s.service.Refresh(r.Context(), r.PathValue("id"))
	if d != nil {
		fail(w, d)
		return
	}
	write(w, http.StatusOK, a)
}
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if d := s.service.Delete(r.PathValue("id")); d != nil {
		fail(w, d)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) aggregate(w http.ResponseWriter, r *http.Request) {
	mode, d := refreshMode(r)
	if d != nil {
		fail(w, d)
		return
	}
	result, d := s.service.Aggregate(r.Context(), mode)
	if d != nil {
		fail(w, d)
		return
	}
	write(w, http.StatusOK, result)
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	mode, d := refreshMode(r)
	if d != nil {
		fail(w, d)
		return
	}
	result, d := s.service.Usage(r.Context(), r.PathValue("id"), mode)
	if d != nil {
		fail(w, d)
		return
	}
	write(w, http.StatusOK, result)
}

func refreshMode(r *http.Request) (string, *model.ErrorDetail) {
	mode := r.URL.Query().Get("refresh")
	if mode == "" {
		mode = "background"
	}
	if mode != "background" && mode != "wait" {
		return "", teach.New(teach.InvalidRequest, "refresh must be background or wait.", "usage", nil, map[string]any{"refresh": mode}, nil, "correct the query")
	}
	return mode, nil
}
func decode(r *http.Request, dst any) *model.ErrorDetail {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return teach.New(teach.InvalidRequest, "The request body is malformed or contains an unknown field.", "accounts", nil, map[string]any{}, nil, "send the documented JSON body")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return teach.New(teach.InvalidRequest, "The request body contains trailing JSON.", "accounts", nil, map[string]any{}, nil, "send one JSON object")
	}
	return nil
}
func emptyBody(r *http.Request) *model.ErrorDetail {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 2))
	if strings.TrimSpace(string(b)) != "" {
		return teach.New(teach.InvalidRequest, "This endpoint takes no request body.", "accounts", nil, map[string]any{}, nil, "retry with an empty body")
	}
	return nil
}
func write(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, d *model.ErrorDetail) {
	write(w, teach.Status(d.Code), model.ErrorEnvelope{Error: d, RequestID: requestID()})
}
func requestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}
