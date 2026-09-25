// Package prepare runs the trusted dependency-preparation stage (F8). When the
// base-branch policy has a prepare object, the policy's command runs once, in
// one bounded container, on the declared inputs exported from the base commit,
// before any candidate code. The container is committed as a local derived
// image, keyed by everything the container sees, and every sandbox run of the
// review then uses that image by ID. A later review with the same key and
// source commit reuses the image without starting a container.
//
// The stage fails closed: every failure leaves no image to run checks in, and
// the review executes nothing else (exit 4). Preparation is environment, never
// a check or evidence, and it supports no hypothesis.
package prepare

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// ExportLimits bound the exported inputs: at most 256 files, 32 MiB per file
// and 128 MiB in total, checked against the tree before any blob is read.
var ExportLimits = gitrepo.ExportLimits{MaxFiles: 256, MaxFileBytes: 32 << 20, MaxTotalBytes: 128 << 20}

// AuditTool is the audit name of the stage (the reserved "stage:" prefix).
const AuditTool = model.AuditStagePrefix + "prepare"

// ArtifactKind is the kind of the retained prepare log.
const ArtifactKind = model.ArtifactPrepareOutput

// Exporter writes the files of a commit whose paths match into a directory.
// *gitrepo.Repository satisfies it.
type Exporter interface {
	ExportMatching(ctx context.Context, commit, dest string, match func(string) bool, limits gitrepo.ExportLimits) ([]gitrepo.ExportedFile, error)
}

// Options configure one preparation.
type Options struct {
	Spec         config.Prepare
	BaseImage    string // sandbox.image as written in the trusted policy
	SourceCommit string // change.BaseCommit: the only commit inputs are exported from
	Repo         Exporter
	// AllowNetwork is the effective permission: policy prepare.network, and
	// --allow-prepare-network, and not --no-network. A miss whose build needs
	// network without this permission is not_permitted.
	AllowNetwork bool
	// Sandbox limits reused by the prepare container and its log.
	MemoryMB, CPUs, MaxOutputBytes int
	ArtifactDir                    string
	ToolVersion                    string
	Docker                         Docker
	Runner                         dockerutil.Runner // image inspect, list and removal
	Progress                       io.Writer         // stderr progress lines; nil discards them
	Now                            func() time.Time
}

// Result is the outcome of one preparation.
type Result struct {
	Record    model.Prepare
	Image     string // the derived image ID every sandbox run uses; "" unless built or reused
	Artifacts []model.Artifact
	Audit     model.AuditEvent
	// Started reports whether the prepare container was started in this run,
	// that is, whether the base-branch prepare command may have run.
	Started bool
	// Shadowed reports that every change the prepare command made is under a
	// location checks replace or do not use (/workspace, /tmp, the prepare
	// HOME), so checks may run without the prepared dependencies.
	Shadowed bool
}

// Ready reports whether the stage produced an image to run checks in.
func (r Result) Ready() bool { return r.Image != "" }

// shadowPrefixes are the container locations whose content checks never see:
// checks mount tmpfs over /workspace and /tmp and set HOME=/tmp.
var shadowPrefixes = []string{"/workspace", "/tmp", HomeDir}

// Run prepares the image. It never pulls, never uses candidate content and
// never falls back to the unprepared image: any failure returns a Result whose
// Ready is false.
func Run(ctx context.Context, o Options) Result {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Progress == nil {
		o.Progress = io.Discard
	}
	// The harness defaults, so that the key and the container agree.
	if o.MemoryMB <= 0 {
		o.MemoryMB = 1024
	}
	if o.CPUs <= 0 {
		o.CPUs = 2
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = 32 << 10
	}
	s := &stage{o: o, started: o.Now()}
	s.res.Record = model.Prepare{
		Status: model.PrepareFailed, SourceCommit: o.SourceCommit, Command: redactAll(o.Spec.Command), User: o.Spec.EffectiveUser(),
		Network: o.Spec.Network && o.AllowNetwork, BaseImage: o.BaseImage, Inputs: []model.PreparedInput{}, Note: model.PrepareNote,
	}
	s.run(ctx)
	s.res.Record.DurationMS = o.Now().Sub(s.started).Milliseconds()
	if s.res.Record.DurationMS < 0 {
		s.res.Record.DurationMS = 0
	}
	s.res.Audit = model.AuditEvent{Time: s.started.UTC(), Tool: AuditTool, Arguments: s.auditArguments(), Status: s.auditStatus, DurationMS: s.res.Record.DurationMS}
	if s.auditStatus == "" {
		s.res.Audit.Status = "ERROR"
	}
	return s.res
}

type stage struct {
	o           Options
	started     time.Time
	res         Result
	auditStatus string
	base        dockerutil.Image
	inputsDir   string
	files       []gitrepo.ExportedFile
	key, tag    string
}

func (s *stage) fail(reason string) {
	s.res.Record.Status, s.res.Record.Reason, s.res.Image, s.res.Record.ImageID = model.PrepareFailed, reason, "", ""
	s.auditStatus = "ERROR"
}

func (s *stage) run(ctx context.Context) {
	o := s.o
	if !validCommit(o.SourceCommit) {
		s.fail("the base commit is not a resolved commit identifier")
		return
	}
	if o.Repo == nil || o.Docker == nil {
		s.fail("dependency preparation has no Git repository or Docker client")
		return
	}
	inspectCtx, cancel := context.WithTimeout(ctx, time.Minute)
	base, found, err := dockerutil.InspectImage(inspectCtx, o.Runner, o.BaseImage)
	cancel()
	if err != nil {
		s.fail(fmt.Sprintf("Docker could not inspect the sandbox image %s: %v", o.BaseImage, err))
		return
	}
	if !found {
		s.fail(fmt.Sprintf("the sandbox image %s is not available locally; SwiftProof never pulls images", o.BaseImage))
		return
	}
	s.base = base
	s.res.Record.BaseImageID = base.ID
	tmp, err := os.MkdirTemp("", "swiftproof-prepare-")
	if err != nil {
		s.fail("the inputs directory could not be created: " + err.Error())
		return
	}
	defer os.RemoveAll(tmp)
	s.inputsDir = filepath.Join(tmp, "inputs")
	if strings.ContainsAny(s.inputsDir, ",\"\r\n") {
		s.fail("the temporary inputs directory path contains a character that docker --mount cannot take: " + s.inputsDir)
		return
	}
	if !s.export(ctx) {
		return
	}
	s.key = Key(o.ToolVersion, base.ID, o.Spec, s.files, o.MemoryMB, o.CPUs)
	s.tag = Tag(s.key, o.SourceCommit)
	s.res.Record.Key = s.key
	if s.reuse(ctx) {
		return
	}
	if o.Spec.Network && !o.AllowNetwork {
		s.res.Record.Status, s.res.Record.Network = model.PrepareNotPermitted, false
		s.res.Record.Reason = fmt.Sprintf("no local image was prepared for key %s… and source commit %s; building it needs the network that prepare.network requests, which also requires --allow-prepare-network (and no --no-network)", prefix(s.key, 12), prefix(o.SourceCommit, 12))
		s.auditStatus = "SKIPPED"
		return
	}
	s.build(ctx)
}

// export writes the declared inputs of the source commit to the inputs
// directory. Credential-bearing matches are refused, never exported.
func (s *stage) export(ctx context.Context) bool {
	var refused []string
	match := func(p string) bool {
		if Matching(s.o.Spec.Inputs, p) == "" {
			return false
		}
		if harness.IsSensitivePath(p) {
			refused = append(refused, p)
			return false
		}
		return true
	}
	files, err := s.o.Repo.ExportMatching(ctx, s.o.SourceCommit, s.inputsDir, match, ExportLimits)
	if len(refused) > 0 {
		sort.Strings(refused)
		s.fail("prepare.inputs match credential-bearing paths, which are never exported: " + listPaths(refused, 20))
		return false
	}
	if err != nil {
		s.fail("the prepare inputs could not be exported from the base commit: " + err.Error())
		return false
	}
	if len(files) == 0 {
		s.fail(fmt.Sprintf("prepare.inputs matched no file at the base commit %s", prefix(s.o.SourceCommit, 12)))
		return false
	}
	s.files = files
	for _, f := range files {
		s.res.Record.Inputs = append(s.res.Record.Inputs, model.PreparedInput{Path: f.Path, SHA256: f.SHA256, Size: f.Size})
	}
	return true
}

// reuse serves the image tagged for this key and source commit when exactly
// one exists and it verifies. Anything else is a miss.
func (s *stage) reuse(ctx context.Context) bool {
	lookupCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ids, err := listImages(lookupCtx, s.o.Runner, s.tag, s.key)
	if err != nil || len(ids) != 1 {
		return false
	}
	img, found, err := dockerutil.InspectImage(lookupCtx, s.o.Runner, ids[0])
	if err != nil || !found || img.ID != ids[0] {
		return false
	}
	if problem := s.verify(img); problem != "" {
		fmt.Fprintf(s.o.Progress, "Not reusing prepared image %s: %s.\n", prefix(img.ID, 19)+"…", problem)
		return false
	}
	added, err := layerSize(lookupCtx, s.o.Runner, img.ID)
	if err != nil {
		return false
	}
	if added > s.maxAdded() {
		fmt.Fprintf(s.o.Progress, "Not reusing prepared image %s: it adds more than prepare.max_added_mb.\n", prefix(img.ID, 19)+"…")
		return false
	}
	r := &s.res.Record
	r.Status, r.ImageID, r.AddedBytes, r.Network = model.PrepareReused, img.ID, added, s.o.Spec.Network
	s.res.Image = img.ID
	s.res.Shadowed = img.Labels[LabelOutputs] == OutputsShadowed
	s.auditStatus = "OK"
	fmt.Fprintf(s.o.Progress, "Reusing prepared image %s (key %s…); the prepare command did not run.\n", prefix(img.ID, 19)+"…", prefix(s.key, 12))
	return true
}

func (s *stage) maxAdded() int64 { return int64(s.o.Spec.EffectiveMaxAddedMB()) << 20 }

// build runs the prepare command in a fresh container and commits it.
func (s *stage) build(ctx context.Context) {
	o := s.o
	name := containerPrefix + randomHex(12)
	s.buildIn(ctx, name)
	// The container is always removed, whatever happened; an unconfirmed
	// removal fails the stage.
	rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.Docker.Remove(rmCtx, name); err != nil {
		if s.res.Record.Status == model.PrepareBuilt {
			s.fail("Docker could not confirm the removal of the prepare container " + name + ": " + err.Error())
		} else {
			s.res.Record.Reason += "; Docker could not confirm the removal of the prepare container " + name
		}
	}
}

func (s *stage) buildIn(ctx context.Context, name string) {
	o := s.o
	memory, cpus, maxOutput := o.MemoryMB, o.CPUs, o.MaxOutputBytes
	network := "disabled"
	if o.Spec.Network {
		network = "enabled"
	}
	s.res.Record.Network = o.Spec.Network
	fmt.Fprintf(o.Progress, "Preparing dependencies in an isolated Docker container (network: %s, user: %s)...\n", network, o.Spec.EffectiveUser())
	args := createArgs(name, s.inputsDir, s.base.ID, o.Spec, o.Spec.Network, memory, cpus)
	runCtx, cancel := context.WithTimeout(ctx, o.Spec.EffectiveTimeout())
	log := &capWriter{limit: maxOutput}
	started, code, runErr := o.Docker.Run(runCtx, name, args, scaffold(), log)
	timedOut := runCtx.Err() != nil
	cancel()
	s.res.Started = started
	logSHA, err := s.saveLog(log.bytes(), log.cut, maxOutput)
	if err != nil {
		s.fail("the prepare log could not be retained as an artifact: " + err.Error())
		return
	}
	s.res.Record.LogSHA256 = logSHA
	switch {
	case timedOut:
		if ctx.Err() != nil {
			s.fail("the overall --deadline was reached during dependency preparation")
		} else {
			s.fail(fmt.Sprintf("the prepare command did not finish within prepare.timeout_seconds (%d s)", int(o.Spec.EffectiveTimeout()/time.Second)))
		}
		s.auditStatus = "TIMEOUT"
		return
	case runErr != nil:
		s.fail("Docker could not run the prepare container: " + runErr.Error())
		return
	case code >= 125 && code <= 127:
		s.fail(fmt.Sprintf("the prepare container could not start the command (exit %d); see the prepare_output log", code))
		return
	case code != 0:
		s.fail(fmt.Sprintf("the prepare command exited %d; see the prepare_output log", code))
		return
	}
	diffCtx, cancelDiff := context.WithTimeout(ctx, time.Minute)
	diff, cut, err := o.Docker.Changed(diffCtx, name)
	cancelDiff()
	if err != nil {
		s.fail("Docker could not list the prepare container's changes: " + err.Error())
		return
	}
	changes := classifyChanges(diff, cut, s.files)
	if len(changes.persistent) == 0 && len(changes.shadowed) == 0 {
		s.fail("the prepare command exited 0 but changed no file outside the directories SwiftProof creates; install dependencies into a persistent image location such as GOMODCACHE, /opt or " + WorkDir)
		return
	}
	outputs := OutputsPersistent
	if len(changes.persistent) == 0 {
		outputs = OutputsShadowed
	}
	labels := map[string]string{
		LabelSchema: Schema, LabelKey: s.key, LabelSourceCommit: o.SourceCommit, LabelBaseImageID: s.base.ID,
		LabelToolVersion: toolVersionLabel(o.ToolVersion), LabelOutputs: outputs, LabelLogSHA256: logSHA,
	}
	id, err := o.Docker.Commit(ctx, name, s.tag, labelChanges(labels))
	if err != nil {
		s.fail("Docker could not commit the prepare container: " + err.Error())
		return
	}
	inspectCtx, cancelInspect := context.WithTimeout(context.Background(), time.Minute)
	defer cancelInspect()
	img, found, err := dockerutil.InspectImage(inspectCtx, o.Runner, id)
	if err != nil || !found || img.ID != id {
		detail := "not found"
		if err != nil {
			detail = err.Error()
		}
		s.fail("Docker could not inspect the committed image " + id + ": " + detail + s.discard(id))
		return
	}
	if problem := s.verify(img); problem != "" {
		s.fail("the committed image did not pass the image checks: " + problem + s.discard(id))
		return
	}
	added, err := layerSize(inspectCtx, o.Runner, id)
	if err != nil {
		s.fail("Docker could not read the size of the committed layer: " + err.Error() + s.discard(id))
		return
	}
	s.res.Record.AddedBytes = added
	if added > s.maxAdded() {
		s.fail(fmt.Sprintf("the derived image adds %d bytes to the base image, over prepare.max_added_mb (%d MiB)", added, o.Spec.EffectiveMaxAddedMB()) + s.discard(id))
		return
	}
	r := &s.res.Record
	r.Status, r.Reason, r.ImageID = model.PrepareBuilt, "", id
	s.res.Image = id
	s.res.Shadowed = outputs == OutputsShadowed
	s.auditStatus = "OK"
}

// discard removes an image this run committed but will not use, and returns
// the sentence the failure reason ends with.
func (s *stage) discard(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := removeImage(ctx, s.o.Runner, id); err != nil {
		return "; the image could not be removed: " + err.Error()
	}
	return "; the image was removed"
}

// verify checks a derived image against this run: labels equal to the full
// key, the source commit and the base image ID, well-formed provenance labels,
// the base image's layers plus exactly one, and the policy env baked in.
func (s *stage) verify(img dockerutil.Image) string {
	l := img.Labels
	switch {
	case !dockerutil.ValidImageID(img.ID):
		return "the image ID is invalid"
	case l[LabelSchema] != Schema:
		return "its schema label is not " + Schema
	case l[LabelKey] != s.key:
		return "its key label differs"
	case l[LabelSourceCommit] != s.o.SourceCommit:
		return "its source-commit label differs from the base commit of this review"
	case l[LabelBaseImageID] != s.base.ID:
		return "its base-image label differs from the sandbox image ID"
	case l[LabelOutputs] != OutputsPersistent && l[LabelOutputs] != OutputsShadowed:
		return "its outputs label is malformed"
	case !toolVersionPattern.MatchString(l[LabelToolVersion]):
		return "its tool-version label is malformed"
	case !sha256Pattern.MatchString(l[LabelLogSHA256]):
		return "its log label is malformed"
	}
	if len(img.Layers) != len(s.base.Layers)+1 {
		return "its layers are not the sandbox image's layers plus one"
	}
	for i, layer := range s.base.Layers {
		if img.Layers[i] != layer {
			return "its layers do not start with the sandbox image's layers"
		}
	}
	env := map[string]string{}
	for _, e := range img.Env {
		name, value, _ := strings.Cut(e, "=")
		env[name] = value
	}
	for _, e := range sortedEnv(s.o.Spec.Env) {
		if value, ok := env[e[0]]; !ok || value != e[1] {
			return "its environment does not set " + e[0] + " to the policy value"
		}
	}
	return ""
}

// saveLog retains the redacted, bounded container output as the hashed
// prepare_output artifact and returns its SHA-256.
func (s *stage) saveLog(data []byte, cut bool, limit int) (string, error) {
	text := strings.ToValidUTF8(string(data), "�")
	if cut {
		text += fmt.Sprintf("\n[swiftproof: prepare output truncated at %d bytes]\n", limit)
	}
	text = redact.Redact(text)
	if err := os.MkdirAll(s.o.ArtifactDir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(s.o.ArtifactDir, "prepare-"+randomHex(8)+".log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write([]byte(text))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	sum := sha256Hex([]byte(text))
	s.res.Artifacts = append(s.res.Artifacts, model.Artifact{Path: path, Kind: ArtifactKind, SHA256: sum})
	return sum, nil
}

func (s *stage) auditArguments() string {
	b, err := json.Marshal(struct {
		Status       string `json:"status"`
		Key          string `json:"key,omitempty"`
		SourceCommit string `json:"source_commit"`
		ImageID      string `json:"image_id,omitempty"`
	}{s.res.Record.Status, s.res.Record.Key, s.res.Record.SourceCommit, s.res.Record.ImageID})
	if err != nil {
		return ""
	}
	return redact.TruncateUTF8(redact.Redact(string(b)), 1024)
}

// changes is what `docker diff` shows beyond what SwiftProof itself created:
// the scaffold directories, the inputs mount point and the copied inputs.
type changes struct {
	persistent, shadowed []string
}

// classifyChanges sorts each `docker diff` line ("A /path", "C /path",
// "D /path") into persistent changes, which checks can see, and shadowed ones,
// under the locations checks replace or do not use. A cut listing is
// classified from the lines it kept; its partial last line is ignored.
func classifyChanges(diff []byte, cut bool, inputs []gitrepo.ExportedFile) changes {
	own := map[string]bool{"/swiftproof": true, WorkDir: true, HomeDir: true}
	for _, f := range inputs {
		for p := WorkDir + "/" + f.Path; p != WorkDir && p != "/"; p = parentPath(p) {
			own[p] = true
		}
	}
	lines := strings.Split(string(diff), "\n")
	if cut && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	var c changes
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		p := l
		if len(l) > 2 && strings.ContainsRune("ACD", rune(l[0])) && l[1] == ' ' {
			p = l[2:]
		}
		switch {
		case own[p] || p == InputsDir || strings.HasPrefix(p, InputsDir+"/"):
		case under(p, shadowPrefixes):
			c.shadowed = append(c.shadowed, p)
		default:
			c.persistent = append(c.persistent, p)
		}
	}
	return c
}

func parentPath(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func under(p string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// labelChanges renders the labels as `docker commit --change` instructions,
// sorted by key. Every value is a fixed token, a hex digest, an image ID or a
// sanitized version, so no quoting is needed.
func labelChanges(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, "LABEL "+k+"="+labels[k])
	}
	return out
}

var (
	toolVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern      = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// toolVersionLabel is the tool version restricted to a label-safe charset.
func toolVersionLabel(v string) string {
	if toolVersionPattern.MatchString(v) {
		return v
	}
	return "unknown"
}

func validCommit(c string) bool { return commitPattern.MatchString(c) }

func redactAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redact.Redact(a)
	}
	return out
}

// listPaths joins up to limit paths and counts the rest.
func listPaths(paths []string, limit int) string {
	if len(paths) <= limit {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(paths[:limit], ", "), len(paths)-limit)
}

// ListPaths joins up to limit paths and counts the rest (the cli's Unverified
// sentences use it).
func ListPaths(paths []string, limit int) string { return listPaths(paths, limit) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
