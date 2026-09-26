package report

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// This file belongs to F8 (trusted dependency preparation). The prepare record
// describes the environment of the review: it is never a check, never
// evidence, supports no hypothesis and changes no exit code here (cli sets
// exit 4 for a preparation that produced no image).

// maxPreparedInputsListed bounds the inputs listed in Markdown; the JSON keeps
// them all.
const maxPreparedInputsListed = 50

// writePrepare renders "## Dependency Preparation" when r.Prepare is present.
// It follows the Change Summary, which already ends with a blank line, and
// ends with a blank line itself. Every string goes through inline().
func writePrepare(b *bytes.Buffer, r *model.Report) {
	p := r.Prepare
	if p == nil {
		return
	}
	line(b, "## Dependency Preparation\n")
	reason := prepareCapitalize(prepareOrNone(strings.TrimSuffix(strings.TrimSpace(p.Reason), ".")))
	switch p.Status {
	case model.PrepareBuilt:
		line(b, "Status: built. The base-branch prepare command ran in this review, before any candidate code, on inputs exported from the source commit, and its container was committed as the image every sandbox run of this review used.\n")
	case model.PrepareReused:
		line(b, "Status: reused. The prepare command did not run in this review: a local image whose labels record this key, source commit and base image ID was used by every sandbox run. Image labels are unsigned local metadata.\n")
	case model.PrepareFailed:
		fmt.Fprintf(b, "Status: failed. %s. No image was used for checks, so no check ran.\n\n", inline(reason))
	case model.PrepareNotPermitted:
		fmt.Fprintf(b, "Status: not permitted. %s. The prepare command did not run and no check ran.\n\n", inline(reason))
	case model.PrepareNotRun:
		fmt.Fprintf(b, "Status: not run. %s.\n\n", inline(reason))
	default:
		fmt.Fprintf(b, "Status: %s. %s.\n\n", inline(p.Status), inline(reason))
	}
	if p.ImageID != "" {
		fmt.Fprintf(b, "Image: %s, derived from %s (%s)", inline(p.ImageID), inline(p.BaseImage), inline(prepareOrNone(p.BaseImageID)))
		if p.AddedBytes > 0 {
			fmt.Fprintf(b, "; adds %d bytes", p.AddedBytes)
		}
		line(b, "\n")
	} else {
		fmt.Fprintf(b, "Sandbox image: %s", inline(p.BaseImage))
		if p.BaseImageID != "" {
			fmt.Fprintf(b, " (%s)", inline(p.BaseImageID))
		}
		line(b, "\n")
	}
	if len(p.Inputs) > 0 {
		fmt.Fprintf(b, "Source commit: %s. The inputs listed below were exported from this commit only; candidate files are never used.\n\n", inline(prepareOrNone(p.SourceCommit)))
	} else {
		fmt.Fprintf(b, "Source commit: %s. No input was exported; inputs are only ever exported from this commit, never from candidate files.\n\n", inline(prepareOrNone(p.SourceCommit)))
	}
	detail := inline(prepareOrNone(p.User))
	if network := prepareNetwork(p); network != "" {
		detail += "; " + network
	}
	if prepareArgsPlain(p.Command) {
		fmt.Fprintf(b, "Command: %s (user %s)\n\n", inline(strings.Join(p.Command, " ")), detail)
	} else {
		// An argument with spaces, or an empty one, is ambiguous when joined:
		// list one argument per line instead.
		fmt.Fprintf(b, "Command, one argument per line (user %s):\n", detail)
		for _, arg := range p.Command {
			if arg == "" {
				line(b, "- (empty argument)")
			} else {
				fmt.Fprintf(b, "- %s\n", inline(arg))
			}
		}
		line(b, "")
	}
	if p.Key != "" {
		fmt.Fprintf(b, "Key: %s\n\n", inline(p.Key))
	}
	if p.LogSHA256 != "" {
		fmt.Fprintf(b, "Prepare log: prepare_output artifact, sha256 %s\n\n", inline(p.LogSHA256))
	}
	if p.Status == model.PrepareBuilt || p.Status == model.PrepareFailed {
		fmt.Fprintf(b, "Duration: %d ms\n\n", p.DurationMS)
	}
	if len(p.Inputs) > 0 {
		fmt.Fprintf(b, "Inputs (%d):\n", len(p.Inputs))
		for i, in := range p.Inputs {
			if i == maxPreparedInputsListed {
				fmt.Fprintf(b, "- … and %d more in confidence-report.json\n", len(p.Inputs)-maxPreparedInputsListed)
				break
			}
			fmt.Fprintf(b, "- %s (sha256 %s; %d bytes)\n", inline(in.Path), inline(in.SHA256), in.Size)
		}
		line(b, "")
	}
	var changed []string
	for _, s := range r.Signals {
		if s.Kind == model.SignalPrepareInputChanged {
			changed = append(changed, s.Path)
		}
	}
	if len(changed) > 0 {
		listed := changed
		if len(listed) > 20 {
			listed = listed[:20]
		}
		fmt.Fprintf(b, "Candidate changes to declared inputs (never installed): %s", inline(strings.Join(listed, ", ")))
		if len(changed) > len(listed) {
			fmt.Fprintf(b, ", and %d more", len(changed)-len(listed))
		}
		line(b, "\n")
	}
	line(b, inline(prepareOrNone(p.Note))+"\n")
}

// finalizePrepare normalizes the prepare note only. It may mutate only
// r.Prepare (F8).
func finalizePrepare(r *model.Report) {
	if r.Prepare != nil && strings.TrimSpace(r.Prepare.Note) == "" {
		r.Prepare.Note = model.PrepareNote
	}
}

// prepareNetwork describes the network bit of the record for its status. The
// bit is the network of the container that built the image, not a setting of
// this review: a reused image was built in an earlier review, and checks
// never get the prepare network. A review that prepared nothing has none.
func prepareNetwork(p *model.Prepare) string {
	state := "disabled"
	if p.Network {
		state = "enabled"
	}
	switch p.Status {
	case model.PrepareBuilt:
		return "network during the build: " + state
	case model.PrepareReused:
		return "the image was built with network " + state
	case model.PrepareNotPermitted:
		return "the build needed network, which was not permitted"
	case model.PrepareNotRun:
		return ""
	default:
		return "network for the build: " + state
	}
}

// prepareArgsPlain reports whether joining args with spaces is unambiguous:
// at least one argument, none empty and none containing white space.
func prepareArgsPlain(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		if a == "" || strings.IndexFunc(a, unicode.IsSpace) >= 0 {
			return false
		}
	}
	return true
}

func prepareOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none recorded"
	}
	return s
}

func prepareCapitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
