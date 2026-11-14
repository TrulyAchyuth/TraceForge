package api

import (
	"net/http"
	"strconv"

	"github.com/TrulyAchyuth/TraceForge/internal/memory"
)

func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sim, err := s.opt.Memory.Similar(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sim)
}

func (s *Server) handleMemoryRecords(w http.ResponseWriter, r *http.Request) {
	recs, err := s.opt.Memory.Records(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs, "method_version": memory.MethodVersion, "weights": memory.Weights()})
}

func (s *Server) handleMemoryGraph(w http.ResponseWriter, r *http.Request) {
	recs, err := s.opt.Memory.Records(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"graph": memory.BuildGraph(recs), "record_count": len(recs)})
}

func (s *Server) handleMemoryCompare(w http.ResponseWriter, r *http.Request) {
	a, errA := strconv.ParseInt(r.URL.Query().Get("a"), 10, 64)
	b, errB := strconv.ParseInt(r.URL.Query().Get("b"), 10, 64)
	if errA != nil || errB != nil || a <= 0 || b <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "a and b must be positive memory record ids")
		return
	}
	ra, err := s.opt.Memory.Get(r.Context(), a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rb, err := s.opt.Memory.Get(r.Context(), b)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, memory.Compare(ra, rb))
}

func (s *Server) handleMemorySeed(w http.ResponseWriter, r *http.Request) {
	n, err := s.opt.Memory.Seed(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"seeded": n, "note": memory.SeedNote})
}

func (s *Server) handleMemoryUnseed(w http.ResponseWriter, r *http.Request) {
	n, err := s.opt.Memory.Unseed(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": n})
}
