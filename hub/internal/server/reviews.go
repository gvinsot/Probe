package server

import (
	"net/http"
	"slices"
	"time"

	"github.com/gvinsot/Probe/hub/internal/store"
)

// handleReview marks the analysis of a commit as reviewed by the signed-in
// person, or withdraws the mark. The report and its CLI verdict are left as
// they are: the mark is a human decision shown beside them.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	if v := r.URL.Query().Get("variant"); v != "" && v != "normal" {
		writeError(w, http.StatusBadRequest, "only an analysis is reviewed")
		return
	}
	repo, rec, ok := s.recordOf(w, r, sess)
	if !ok {
		return
	}
	var body struct {
		Reviewed bool `json:"reviewed"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rec.Status != store.StatusDone {
		writeError(w, http.StatusConflict, "only a finished analysis can be reviewed")
		return
	}
	entry := store.ReviewEntry{
		Commit: rec.Commit, Message: rec.Message, Verdict: rec.Summary.Verdict,
		Reviewed: body.Reviewed, By: sess.Login, At: time.Now().UTC(),
	}
	updated, err := s.store.UpdateRepo(sess.UserKey, repo.Key, func(repo *store.Repo) error {
		// Withdrawing a mark that is not there records nothing.
		if _, marked := repo.Reviewed()[entry.Commit]; marked != entry.Reviewed {
			repo.AddReview(entry)
		}
		return nil
	})
	if err != nil {
		s.log.Error("store review", "error", err)
		writeError(w, http.StatusInternalServerError, "could not record the review")
		return
	}
	s.events.Publish(sess.UserKey, map[string]any{"type": "repo", "repo": updated.Public()})
	writeJSON(w, http.StatusOK, map[string]any{"repo": updated.Public()})
}

// handleReviews lists the review history of a repository, newest first.
func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	repo, ok := s.repoOf(w, r, sess)
	if !ok {
		return
	}
	reviews := slices.Clone(repo.Reviews)
	slices.Reverse(reviews)
	if reviews == nil {
		reviews = []store.ReviewEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews})
}
