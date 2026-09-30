package coverage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// lcovFields are the record fields an LCOV tracefile may carry (geninfo(1)).
// Only SF, DA and end_of_record are read; the others are accepted and ignored.
var lcovFields = map[string]bool{
	"TN": true, "SF": true, "VER": true, "FN": true, "FNDA": true, "FNF": true, "FNH": true, "FNL": true, "FNA": true,
	"BRDA": true, "BRF": true, "BRH": true, "DA": true, "LF": true, "LH": true,
}

// lcovStart reports whether a line can open an LCOV tracefile.
func lcovStart(text string) bool {
	return strings.HasPrefix(text, "TN:") || strings.HasPrefix(text, "SF:") || strings.HasPrefix(text, "VER:")
}

// ParseLCOV parses an LCOV tracefile, as the Istanbul reporters of Vitest and
// Jest write it. Each DA:<line>,<count>[,<checksum>] entry becomes a one-line
// block of the file its SF record names; the path is kept byte for
// byte and never resolved against the filesystem.
//
// As for a Go profile, a malformed line fails the whole report and no line is
// ever skipped: an unknown field, a DA outside a record or a record that is
// not closed by end_of_record means the report is not the one the tool wrote,
// or was cut short, and a partial report would read as "not executed".
func ParseLCOV(b []byte) (*Profile, error) {
	if len(b) > maxProfileBytes {
		return nil, ErrBlockLimit
	}
	profile := &Profile{Format: FormatLCOV}
	index := map[string]int{}
	file, open, records := "", false, 0
	for row, raw := range strings.Split(string(b), "\n") {
		text := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		malformed := fmt.Errorf("the LCOV report could not be parsed at line %d", row+1)
		if text == "end_of_record" {
			if !open {
				return nil, malformed
			}
			open, file = false, ""
			continue
		}
		field, value, ok := strings.Cut(text, ":")
		if !ok || !lcovFields[field] {
			return nil, malformed
		}
		switch field {
		case "SF":
			if open || value == "" {
				return nil, malformed
			}
			open, file = true, value
			records++
		case "DA":
			if !open {
				return nil, malformed
			}
			line, executions, err := lcovLine(value)
			if err != nil {
				return nil, malformed
			}
			key := fmt.Sprintf("%s:%d", file, line)
			if at, seen := index[key]; seen {
				// A file can appear in several records (one per test process);
				// summing cannot turn an executed line into an unexecuted one.
				profile.Blocks[at].Count += executions
				continue
			}
			if len(profile.Blocks) >= maxBlocks {
				return nil, ErrBlockLimit
			}
			index[key] = len(profile.Blocks)
			profile.Blocks = append(profile.Blocks, Block{File: file, StartLine: line, EndLine: line, NumStmts: 1, Count: executions})
		default:
			if field != "TN" && field != "VER" && !open {
				return nil, malformed
			}
		}
	}
	if open {
		return nil, ErrTruncated
	}
	if records == 0 {
		return nil, ErrFormat
	}
	sort.Slice(profile.Blocks, func(i, j int) bool {
		a, b := profile.Blocks[i], profile.Blocks[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.StartLine < b.StartLine
	})
	return profile, nil
}

// lcovLine reads "<line>,<count>" with an optional trailing checksum. Counts
// past the int range saturate: only zero versus nonzero matters.
func lcovLine(value string) (int, int, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, errSyntax
	}
	line, err := count(parts[0])
	if err != nil || line < 1 {
		return 0, 0, errSyntax
	}
	executions, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		if numErr, ok := err.(*strconv.NumError); !ok || numErr.Err != strconv.ErrRange {
			return 0, 0, errSyntax
		}
		executions = 1 << 30
	}
	if executions > 1<<30 {
		executions = 1 << 30
	}
	return line, int(executions), nil
}
