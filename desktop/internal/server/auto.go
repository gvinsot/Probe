package server

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/reviewer"
	"github.com/gvinsot/Probe/desktop/internal/watch"
)

// errKeychain reports an API key that cannot be read.
var errKeychain = errors.New("cannot read the API key from the keychain")

// explainDocument asks the configured AI provider to explain the report of a
// document and stores the answer, unless the document changed meanwhile.
func (s *Server) explainDocument(ctx context.Context, d watch.Document) (watch.Explanation, error) {
	st := s.deps.Settings.Get()
	key, err := s.deps.Keys.Get(st.Provider)
	if err != nil {
		return watch.Explanation{}, errors.Join(errKeychain, err)
	}
	res, err := reviewer.Explain(ctx, st, key, d.Name, d.Report)
	if err != nil {
		s.deps.Log.Warn("explanation failed", "provider", st.Provider, "model", st.EffectiveModel(), "err", err)
		return watch.Explanation{}, err
	}
	e := watch.Explanation{
		Provider: st.Provider, Model: st.EffectiveModel(), Text: res.Text, Findings: res.Findings,
		Impacts: res.Impacts, Severity: res.Severity, At: time.Now(),
	}
	return e, s.deps.Watcher.SetExplanation(d.ID, d.CurrentHash, e)
}

// autoExplainer explains the changed documents as soon as they are detected,
// when an AI provider is configured. It works on one document at a time, most
// severe first, and does not try a version again after a failure: a new
// version of the document or new settings do.
type autoExplainer struct {
	s      *Server
	kick   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	active  map[string]int    // documents being explained, by id
	tried   map[string]string // last version attempted, by id
	started bool
}

func newAutoExplainer(s *Server) *autoExplainer {
	ctx, cancel := context.WithCancel(context.Background())
	return &autoExplainer{
		s: s, kick: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		active: map[string]int{}, tried: map[string]string{},
	}
}

// Kick asks for a pass over the documents waiting for an explanation. It
// never blocks; the first call starts the worker.
func (s *Server) Kick() {
	a := s.auto
	a.mu.Lock()
	if !a.started {
		a.started = true
		go a.loop()
	}
	a.mu.Unlock()
	select {
	case a.kick <- struct{}{}:
	default: // a pass is already pending
	}
}

func (a *autoExplainer) loop() {
	defer close(a.done)
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.kick:
			a.pass()
		}
	}
}

func (a *autoExplainer) pass() {
	for _, p := range a.s.deps.Watcher.NeedingExplanation() {
		if a.ctx.Err() != nil || !a.s.aiConfigured(a.s.deps.Settings.Get()) {
			return
		}
		a.mu.Lock()
		skip := a.tried[p.ID] == p.Hash || a.active[p.ID] > 0
		if !skip {
			a.tried[p.ID] = p.Hash
		}
		a.mu.Unlock()
		if skip {
			continue
		}
		d, ok := a.s.deps.Watcher.Document(p.ID)
		if !ok || d.CurrentHash != p.Hash || d.Report == nil {
			continue
		}
		a.begin(p.ID)
		_, err := a.s.explainDocument(a.ctx, d)
		a.end(p.ID)
		if err != nil && !errors.Is(err, watch.ErrStale) && a.ctx.Err() == nil {
			a.s.deps.Log.Warn("automatic explanation skipped", "document", d.Path, "err", err)
		}
	}
}

// retry forgets the failed attempts and starts a pass.
func (a *autoExplainer) retry() {
	a.mu.Lock()
	a.tried = map[string]string{}
	started := a.started
	a.mu.Unlock()
	if started {
		a.s.Kick()
	}
}

func (a *autoExplainer) begin(id string) {
	a.mu.Lock()
	a.active[id]++
	a.mu.Unlock()
}

func (a *autoExplainer) end(id string) {
	a.mu.Lock()
	if a.active[id]--; a.active[id] <= 0 {
		delete(a.active, id)
	}
	a.mu.Unlock()
}

// running lists the documents being explained.
func (a *autoExplainer) running() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.active))
	for id := range a.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// stop cancels the explanation in progress and waits for the worker.
func (a *autoExplainer) stop() {
	a.cancel()
	a.mu.Lock()
	started := a.started
	a.mu.Unlock()
	if started {
		<-a.done
	}
}
