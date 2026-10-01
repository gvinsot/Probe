package server

// Device authorization (RFC 8628) for `probe login`: the CLI asks for a code,
// the user opens /device.html in a browser where they are signed in, checks
// that the request names the machine they just ran the command on, and
// approves it. The CLI, polling meanwhile, then receives an llm-scoped agent
// token for that account. No secret ships in the binary: the token stands
// for one account, is limited and counted as that account, and is revoked
// like any other agent token.
//
// Pending requests live in memory for ten minutes. The hub runs as a single
// replica; a restart only makes a pending login start again.

import (
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gvinsot/Probe/hub/internal/secrets"
	"github.com/gvinsot/Probe/hub/internal/store"
)

const (
	deviceCodeTTL      = 10 * time.Minute
	devicePollInterval = 5 * time.Second
	// maxPendingDevices bounds the requests waiting for approval.
	maxPendingDevices = 1000
	// deviceTokenDays is the lifetime of a token issued to a login.
	deviceTokenDays = 90
	// maxDeviceClient bounds the client description shown on approval.
	maxDeviceClient = 60
	// deviceCodeRate bounds the codes issued per minute, all callers together:
	// the endpoint is anonymous.
	deviceCodeRate = 120
	// deviceLookupRate bounds the codes one account may look up per minute,
	// which keeps the user-code space out of reach of guessing.
	deviceLookupRate = 30
	// userCodeAlphabet has no vowels (no words) and no look-alike characters.
	userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
	userCodeLength   = 8
)

type deviceGrant struct {
	userCode string
	client   string
	expires  time.Time
	interval time.Duration
	lastPoll time.Time
	// userKey is the approving account; denied records a refusal.
	userKey string
	denied  bool
}

// deviceFlows holds the pending requests, keyed by the hash of the device
// code (the CLI's secret) and by the user code (what the user types).
type deviceFlows struct {
	mu       sync.Mutex
	byDevice map[string]*deviceGrant
	byUser   map[string]string
	codes    *windowLimiter
	lookups  *windowLimiter
}

func newDeviceFlows() *deviceFlows {
	return &deviceFlows{
		byDevice: map[string]*deviceGrant{},
		byUser:   map[string]string{},
		codes:    newWindowLimiter(deviceCodeRate, time.Minute),
		lookups:  newWindowLimiter(deviceLookupRate, time.Minute),
	}
}

var errDeviceFull = errors.New("too many pending logins")

// start registers a request and returns its device and user codes.
func (d *deviceFlows) start(client string, now time.Time) (string, string, error) {
	device, err := secrets.Random(32)
	if err != nil {
		return "", "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweep(now)
	if len(d.byDevice) >= maxPendingDevices {
		return "", "", errDeviceFull
	}
	var user string
	for {
		if user, err = newUserCode(); err != nil {
			return "", "", err
		}
		if _, taken := d.byUser[user]; !taken {
			break
		}
	}
	hash := hashAgentToken(device)
	d.byDevice[hash] = &deviceGrant{userCode: user, client: client, expires: now.Add(deviceCodeTTL), interval: devicePollInterval}
	d.byUser[user] = hash
	return device, user, nil
}

// sweep forgets expired requests. The caller holds the lock.
func (d *deviceFlows) sweep(now time.Time) {
	for hash, g := range d.byDevice {
		if now.After(g.expires) {
			delete(d.byUser, g.userCode)
			delete(d.byDevice, hash)
		}
	}
}

// pending returns a copy of the live request a user code names.
func (d *deviceFlows) pending(userCode string, now time.Time) (deviceGrant, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	g := d.byUser[userCode]
	grant, ok := d.byDevice[g]
	if !ok || now.After(grant.expires) || grant.userKey != "" || grant.denied {
		return deviceGrant{}, false
	}
	return *grant, true
}

// decide records the user's answer on a live request.
func (d *deviceFlows) decide(userCode, userKey string, approve bool, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	grant, ok := d.byDevice[d.byUser[userCode]]
	if !ok || now.After(grant.expires) || grant.userKey != "" || grant.denied {
		return false
	}
	if approve {
		grant.userKey = userKey
	} else {
		grant.denied = true
	}
	return true
}

// poll answers the CLI: the RFC 8628 error code while there is nothing to
// hand over, or the approving account, once: the request is then forgotten.
func (d *deviceFlows) poll(device string, now time.Time) (deviceGrant, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	hash := hashAgentToken(device)
	grant, ok := d.byDevice[hash]
	switch {
	case !ok:
		return deviceGrant{}, "invalid_grant"
	case now.After(grant.expires):
		delete(d.byUser, grant.userCode)
		delete(d.byDevice, hash)
		return deviceGrant{}, "expired_token"
	case grant.denied:
		delete(d.byUser, grant.userCode)
		delete(d.byDevice, hash)
		return deviceGrant{}, "access_denied"
	case grant.userKey == "":
		if now.Sub(grant.lastPoll) < grant.interval {
			// RFC 8628 §3.5: a client polling too fast waits 5 s longer.
			grant.interval += 5 * time.Second
			grant.lastPoll = now
			return deviceGrant{}, "slow_down"
		}
		grant.lastPoll = now
		return deviceGrant{}, "authorization_pending"
	}
	delete(d.byUser, grant.userCode)
	delete(d.byDevice, hash)
	return *grant, ""
}

func newUserCode() (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(userCodeAlphabet)))
	for i := 0; i < userCodeLength; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(userCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// normalizeUserCode accepts what a user types: any case, with or without
// the dash or spaces.
func normalizeUserCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) != userCodeLength || strings.Trim(out, userCodeAlphabet) != "" {
		return ""
	}
	return out
}

func displayUserCode(code string) string {
	return code[:4] + "-" + code[4:]
}

// deviceClient cleans the description the CLI sends: printable, one line,
// bounded. It is shown to the user, never trusted.
func deviceClient(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	client := strings.Join(strings.Fields(b.String()), " ")
	for utf8.RuneCountInString(client) > maxDeviceClient {
		_, size := utf8.DecodeLastRuneInString(client)
		client = client[:len(client)-size]
	}
	if client == "" {
		client = "Probe CLI"
	}
	return client
}

// handleDeviceCode starts a login. It is anonymous, like the first step of
// any device flow: the code is worthless until a signed-in user approves it.
func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.cfg.Gateway.Enabled {
		writeError(w, http.StatusNotFound, "this deployment does not lend its LLM")
		return
	}
	if !s.devices.codes.Allow("code") {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many logins in progress; try again in a minute")
		return
	}
	var body struct {
		Client string `json:"client"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	device, user, err := s.devices.start(deviceClient(body.Client), time.Now())
	if errors.Is(err, errDeviceFull) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many logins in progress; try again in a minute")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the login")
		return
	}
	page := strings.TrimRight(s.cfg.BaseURL, "/") + "/device.html"
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               device,
		"user_code":                 displayUserCode(user),
		"verification_uri":          page,
		"verification_uri_complete": page + "?code=" + displayUserCode(user),
		"expires_in":                int(deviceCodeTTL.Seconds()),
		"interval":                  int(devicePollInterval.Seconds()),
	})
}

// handleDeviceToken is the CLI's poll. Errors follow RFC 8628 §3.5 so that
// any device-flow client understands them.
func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.cfg.Gateway.Enabled {
		writeError(w, http.StatusNotFound, "this deployment does not lend its LLM")
		return
	}
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := decodeBody(r, &body); err != nil || body.DeviceCode == "" || len(body.DeviceCode) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	now := time.Now()
	grant, code := s.devices.poll(body.DeviceCode, now)
	if code != "" {
		writeError(w, http.StatusBadRequest, code)
		return
	}
	user, err := s.store.User(grant.userKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "access_denied")
		return
	}
	token, record, err := s.issueAgentToken(user.Key, "probe login: "+grant.client, store.ScopeLLM, deviceTokenDays, true)
	if errors.Is(err, errTooManyTokens) {
		writeError(w, http.StatusConflict, "this account already has 20 agent tokens; revoke one in the hub first")
		return
	}
	if err != nil {
		s.log.Error("issue login token", "error", err)
		writeError(w, http.StatusInternalServerError, "could not issue the token")
		return
	}
	s.log.Info("cli login", "user", user.Login, "client", grant.client)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(record.ExpiresAt).Seconds()),
		"endpoint":     s.gatewayURL(),
		"model":        s.cfg.Gateway.Model,
		"login":        user.Login,
		"provider":     user.Provider,
	})
}

// handleDeviceLookup shows the signed-in user what asks for access, before
// they approve it.
func (s *Server) handleDeviceLookup(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.requireBrowser(w, r)
	if !ok {
		return
	}
	if !s.devices.lookups.Allow(sess.UserKey) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many codes tried; wait a minute")
		return
	}
	code := normalizeUserCode(r.URL.Query().Get("code"))
	grant, found := s.devices.pending(code, time.Now())
	if code == "" || !found {
		writeError(w, http.StatusNotFound, "unknown or expired code: run probe login again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user_code": displayUserCode(code), "client": grant.client, "expires_at": grant.expires.UTC()})
}

// handleDeviceDecide approves or denies a pending login for the signed-in
// account.
func (s *Server) handleDeviceDecide(w http.ResponseWriter, r *http.Request) {
	sess, user, ok := s.requireBrowser(w, r)
	if !ok {
		return
	}
	var body struct {
		UserCode string `json:"user_code"`
		Approve  bool   `json:"approve"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	code := normalizeUserCode(body.UserCode)
	if code == "" || !s.devices.decide(code, sess.UserKey, body.Approve, time.Now()) {
		writeError(w, http.StatusNotFound, "unknown or expired code: run probe login again")
		return
	}
	status := "denied"
	if body.Approve {
		status = "approved"
		s.log.Info("cli login approved", "user", user.Login)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// requireBrowser is require for a signed-in browser only: an agent token
// never approves a login.
func (s *Server) requireBrowser(w http.ResponseWriter, r *http.Request) (secrets.Session, *store.User, bool) {
	sess, user, ok := s.require(w, r)
	if !ok {
		return sess, user, false
	}
	if agentOf(r.Context()) != nil {
		writeError(w, http.StatusForbidden, "logins are approved from the dashboard")
		return sess, user, false
	}
	return sess, user, true
}
