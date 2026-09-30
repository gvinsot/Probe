// Package coverage resolves changed lines against a recorded Go coverage
// profile or LCOV report. Executed means a line ran at least once during the recorded run; it
// is never a claim that behavior is asserted, correct or safe. Absent,
// truncated, unparsable or unmapped data always resolves to "not measured" and
// never to a not-executed claim.
package coverage

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// ProfilePath is the fixed in-container path a coverage command must write to.
// /tmp is a fresh tmpfs per run and the snapshot is copied to /workspace, so a
// committed or stale profile cannot occupy it and no discovery is needed.
const ProfilePath = "/tmp/probe-coverage.out"

// ReportDir is the fixed in-container directory a coverage command given
// DirPlaceholder writes its reports to. Tools that write LCOV into a directory
// (Vitest, Jest) name the file lcov.info, and only LCOVPath is captured.
const (
	ReportDir = "/tmp/probe-coverage"
	LCOVPath  = ReportDir + "/lcov.info"
)

const (
	// CommandKey names the optional trusted policy command.
	CommandKey = "coverage"
	// Placeholder is the profile file path. The command's argv holds exactly
	// one occurrence of Placeholder or of DirPlaceholder, never both.
	Placeholder = "{coverage_out}"
	// DirPlaceholder is the report directory of a tool that writes lcov.info.
	DirPlaceholder = "{coverage_dir}"
)

// Expand returns the executed argv of a coverage command and the in-container
// file its payload is captured from. Probe expands the token the operator
// wrote and never appends a flag of its own, so the executed argv is the
// reviewed argv.
func Expand(command []string) ([]string, string) {
	out := append([]string(nil), command...)
	capture := ProfilePath
	for i, arg := range out {
		if strings.Contains(arg, DirPlaceholder) {
			capture = LCOVPath
		}
		out[i] = strings.ReplaceAll(strings.ReplaceAll(arg, Placeholder, ProfilePath), DirPlaceholder, ReportDir)
	}
	return out, capture
}

// FrameHeader precedes the decimal payload length and a newline; FrameFooter
// must match exactly at the declared offset. The sandbox wrapper that emits the
// frame is built from these constants so producer and decoder cannot drift.
const (
	FrameHeader = "PROBE-COVERAGE-BEGIN "
	FrameFooter = "PROBE-COVERAGE-END\n"
)

// Every error message is the exact sentence reported to the user, so a caller
// records err.Error() as the reason a measurement did not happen.
var (
	ErrNoProfile  = errors.New("no coverage profile was emitted by the coverage command")
	ErrTruncated  = errors.New("the coverage profile did not fit in the sandbox payload budget or was cut short; raise sandbox.max_output_bytes")
	ErrPolluted   = errors.New("the coverage payload channel carried unexpected output")
	ErrFormat     = errors.New("the coverage profile is neither a Go coverage profile nor an LCOV report; only these two formats are supported in this version")
	ErrBlockLimit = errors.New("the coverage profile exceeded the parser bound of 200000 blocks")
	ErrModulePath = errors.New("the module path could not be read from go.mod or go.work in the candidate snapshot")
	ErrArtifact   = errors.New("the coverage profile could not be retained as evidence")
)

// DecodeFrame returns the profile bytes of the single length-declared frame on
// the payload channel. The declared length is verified by exact arithmetic and
// never by searching for the terminator: a payload cut short must fail loudly,
// because a silently shortened profile parses as "these lines never ran".
func DecodeFrame(payload []byte, truncated bool) ([]byte, error) {
	if truncated {
		return nil, ErrTruncated
	}
	start := bytes.Index(payload, []byte(FrameHeader))
	if start < 0 {
		return nil, ErrNoProfile
	}
	if len(bytes.TrimSpace(payload[:start])) != 0 {
		return nil, ErrPolluted
	}
	rest := payload[start+len(FrameHeader):]
	newline := bytes.IndexByte(rest, '\n')
	if newline < 0 {
		return nil, ErrTruncated
	}
	declared, err := strconv.Atoi(string(rest[:newline]))
	if err != nil || declared < 1 {
		return nil, ErrPolluted
	}
	body := rest[newline+1:]
	if len(body) != declared+len(FrameFooter) || !bytes.Equal(body[declared:], []byte(FrameFooter)) {
		return nil, ErrTruncated
	}
	profile := body[:declared]
	// A second header inside the declared payload means something other than the
	// wrapper wrote to the channel; refuse rather than pick a winner.
	if bytes.Contains(profile, []byte(FrameHeader)) {
		return nil, ErrPolluted
	}
	return profile, nil
}
