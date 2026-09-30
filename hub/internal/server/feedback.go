package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gvinsot/Probe/hub/internal/learning"
	"github.com/gvinsot/Probe/hub/internal/report"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// maxFeedbackComment bounds one comment or reply.
const maxFeedbackComment = 2000

// feedbackView is what the browser receives about a report's feedback.
type feedbackView struct {
	Learning bool                  `json:"learning"`
	Entries  []store.FeedbackEntry `json:"entries"`
}

// feedbackOf lists the entries recorded on one commit, oldest first.
func feedbackOf(repo *store.Repo, commit string) feedbackView {
	view := feedbackView{Learning: !repo.LearningOff, Entries: []store.FeedbackEntry{}}
	for _, e := range repo.Feedback {
		if e.Commit == commit {
			view.Entries = append(view.Entries, e)
		}
	}
	return view
}

// handleFeedback lists the votes, comments and replies on a report.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	repo, rec, ok := s.recordOf(w, r, sess)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, feedbackOf(repo, rec.Commit))
}

// handleAddFeedback records a vote, a comment, or both, on one finding of a
// stored report, or a reply to an earlier comment. What the finding is about
// comes from the stored report, never from the request.
func (s *Server) handleAddFeedback(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	repo, rec, ok := s.recordOf(w, r, sess)
	if !ok {
		return
	}
	var body struct {
		AlertID string `json:"alert_id"`
		Vote    string `json:"vote"`
		Comment string `json:"comment"`
		ReplyTo string `json:"reply_to"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	comment := strings.TrimSpace(strings.ReplaceAll(body.Comment, "\r\n", "\n"))
	switch {
	case body.Vote != "" && body.Vote != store.VoteUp && body.Vote != store.VoteDown:
		writeError(w, http.StatusBadRequest, `vote is "up", "down" or empty`)
		return
	case body.Vote == "" && comment == "":
		writeError(w, http.StatusBadRequest, "a vote or a comment is required")
		return
	case body.ReplyTo != "" && comment == "":
		writeError(w, http.StatusBadRequest, "a reply needs a comment")
		return
	case len(comment) > maxFeedbackComment || !utf8.ValidString(comment) || strings.ContainsRune(comment, 0):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("a comment is UTF-8 text of at most %d bytes", maxFeedbackComment))
		return
	}
	if rec.Variant == "plan" || len(rec.Raw) == 0 {
		writeError(w, http.StatusConflict, "only a finished review report takes feedback")
		return
	}
	parsed, err := report.Decode(rec.Raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the stored report could not be read")
		return
	}
	alert, found := learning.Find(parsed, body.AlertID)
	if !found {
		writeError(w, http.StatusNotFound, "no such finding in this report")
		return
	}
	id, err := feedbackID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the feedback")
		return
	}
	entry := store.FeedbackEntry{
		ID: id, Commit: rec.Commit, AlertID: alert.ID, Topic: learning.TopicOf(alert),
		Path: alert.Path, Title: alert.Title, Vote: body.Vote, Comment: comment,
		ReplyTo: body.ReplyTo, Author: sess.Login, At: time.Now().UTC(),
	}
	updated, err := s.store.UpdateRepo(sess.UserKey, repo.Key, func(repo *store.Repo) error {
		if entry.ReplyTo != "" && !slices.ContainsFunc(repo.Feedback, func(e store.FeedbackEntry) bool {
			return e.ID == entry.ReplyTo && e.Commit == entry.Commit && e.AlertID == entry.AlertID
		}) {
			return errUnknownParent
		}
		repo.Feedback = append(repo.Feedback, entry)
		if extra := len(repo.Feedback) - store.MaxFeedback; extra > 0 {
			repo.Feedback = repo.Feedback[extra:]
		}
		return nil
	})
	if errors.Is(err, errUnknownParent) {
		writeError(w, http.StatusNotFound, "the comment this replies to no longer exists")
		return
	}
	if err != nil {
		s.log.Error("store feedback", "error", err)
		writeError(w, http.StatusInternalServerError, "could not record the feedback")
		return
	}
	s.events.Publish(sess.UserKey, map[string]any{"type": "repo", "repo": updated.Public()})
	writeJSON(w, http.StatusCreated, map[string]any{"entry": entry, "feedback": feedbackOf(updated, rec.Commit)})
}

var errUnknownParent = errors.New("unknown parent comment")

// handleLearning switches learning from team feedback on or off. While it is
// off, feedback is neither collected from outcomes nor given to the reviewer;
// what was kept stays until it is reset.
func (s *Server) handleLearning(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	repo, ok := s.repoOf(w, r, sess)
	if !ok {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.updateLearning(w, sess.UserKey, repo.Key, func(repo *store.Repo) { repo.LearningOff = !body.Enabled })
}

// handleResetFeedback forgets every vote, comment and outcome of a repository.
func (s *Server) handleResetFeedback(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	repo, ok := s.repoOf(w, r, sess)
	if !ok {
		return
	}
	s.updateLearning(w, sess.UserKey, repo.Key, func(repo *store.Repo) {
		repo.Feedback, repo.Outcomes, repo.OutcomeBases = nil, nil, nil
	})
}

// handleLearned shows what the reviewer would receive for a repository.
func (s *Server) handleLearned(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.require(w, r)
	if !ok {
		return
	}
	repo, ok := s.repoOf(w, r, sess)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"learning": !repo.LearningOff, "feedback": learning.Summarize(repo)})
}

func (s *Server) updateLearning(w http.ResponseWriter, userKey, repoKey string, mutate func(*store.Repo)) {
	updated, err := s.store.UpdateRepo(userKey, repoKey, func(repo *store.Repo) error {
		mutate(repo)
		return nil
	})
	if err != nil {
		s.log.Error("update repository", "error", err)
		writeError(w, http.StatusInternalServerError, "the learning settings could not be saved")
		return
	}
	s.events.Publish(userKey, map[string]any{"type": "repo", "repo": updated.Public()})
	writeJSON(w, http.StatusOK, map[string]any{"repo": updated.Public(), "feedback": learning.Summarize(updated)})
}

func feedbackID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "fb-" + hex.EncodeToString(b), nil
}
