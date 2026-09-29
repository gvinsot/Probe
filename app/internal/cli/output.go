package cli

// Export options of lint, review and report (F9).

import (
	"errors"
	"fmt"

	"github.com/gvinsot/Probe/app/internal/report"
)

// reportOptions validates --report-url against the requested formats and
// returns the report.Write options. A URL is accepted only together with the
// pr-comment format and only when report.ValidateReportURL accepts it (https,
// a host, no user information, at most 512 bytes, a character set that cannot
// break out of a Markdown link). Its error exits 3 before anything runs; the
// URL is never fetched.
func reportOptions(formats []string, url string) ([]report.Option, error) {
	if url == "" {
		return nil, nil
	}
	requested := false
	for _, f := range formats {
		if f == report.FormatPRComment {
			requested = true
		}
	}
	if !requested {
		return nil, errors.New("--report-url requires --format pr-comment")
	}
	if err := report.ValidateReportURL(url); err != nil {
		return nil, fmt.Errorf("--report-url: %v", err)
	}
	return []report.Option{report.WithReportURL(url)}, nil
}
