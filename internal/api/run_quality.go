package api

import (
	"context"
	"math"
	"sort"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// QualityCounterJSON is one covered and total count. Percent is the
// covered count times 100 over the total, rounded to one decimal. It is
// absent when the total is zero.
type QualityCounterJSON struct {
	Covered int32    `json:"covered"`
	Total   int32    `json:"total"`
	Percent *float64 `json:"percent,omitempty"`
}

// CoverageDeltaJSON compares a coverage report with the same report of the
// newest earlier run of the same job. LinePoints and BranchPoints are
// percentage-point differences. Each is absent when either side lacks the
// counter.
type CoverageDeltaJSON struct {
	BaseRunID       string   `json:"baseRunId"`
	BaseBuildNumber *int64   `json:"baseBuildNumber,omitempty"`
	LinePoints      *float64 `json:"linePoints,omitempty"`
	BranchPoints    *float64 `json:"branchPoints,omitempty"`
}

// CoverageReportJSON is one coverage report of a run. Delta is absent when
// the run has no earlier run of the same job with the same report.
type CoverageReportJSON struct {
	ID            string                        `json:"id"`
	Name          string                        `json:"name"`
	Line          *QualityCounterJSON           `json:"line,omitempty"`
	Branch        *QualityCounterJSON           `json:"branch,omitempty"`
	ModifiedLines *QualityCounterJSON           `json:"modifiedLines,omitempty"`
	Metrics       map[string]QualityCounterJSON `json:"metrics,omitempty"`
	GateStatus    string                        `json:"gateStatus,omitempty"`
	Delta         *CoverageDeltaJSON            `json:"delta,omitempty"`
}

// WarningsDeltaJSON compares a warnings report with the same report of the
// newest earlier run of the same job. Total is the difference of the two
// totals.
type WarningsDeltaJSON struct {
	BaseRunID       string `json:"baseRunId"`
	BaseBuildNumber *int64 `json:"baseBuildNumber,omitempty"`
	Total           int32  `json:"total"`
}

// WarningsReportJSON is one warnings report of a run. New and Fixed are
// absent when the plugin found no reference build. Delta is absent when the
// run has no earlier run of the same job with the same report.
type WarningsReportJSON struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Total      int32              `json:"total"`
	Error      int32              `json:"error"`
	High       int32              `json:"high"`
	Normal     int32              `json:"normal"`
	Low        int32              `json:"low"`
	New        *int32             `json:"new,omitempty"`
	Fixed      *int32             `json:"fixed,omitempty"`
	GateStatus string             `json:"gateStatus,omitempty"`
	Delta      *WarningsDeltaJSON `json:"delta,omitempty"`
}

// RunQualityJSON is the quality summary of one run. It is absent when the
// run holds no quality row.
type RunQualityJSON struct {
	Coverage []CoverageReportJSON `json:"coverage,omitempty"`
	Warnings []WarningsReportJSON `json:"warnings,omitempty"`
}

// CoverageTrendPointJSON is one run of a coverage trend series. A percent
// is absent when its counter is absent or its total is zero.
type CoverageTrendPointJSON struct {
	RunID         string   `json:"runId"`
	BuildNumber   *int64   `json:"buildNumber,omitempty"`
	LinePercent   *float64 `json:"linePercent,omitempty"`
	BranchPercent *float64 `json:"branchPercent,omitempty"`
}

// CoverageTrendSeriesJSON is the trend of one coverage report over the
// health window. Points are newest first.
type CoverageTrendSeriesJSON struct {
	ID     string                   `json:"id"`
	Name   string                   `json:"name"`
	Points []CoverageTrendPointJSON `json:"points"`
}

// WarningsTrendPointJSON is one run of a warnings trend series. New is
// absent when the plugin found no reference build for that run.
type WarningsTrendPointJSON struct {
	RunID       string `json:"runId"`
	BuildNumber *int64 `json:"buildNumber,omitempty"`
	Total       int32  `json:"total"`
	New         *int32 `json:"new,omitempty"`
}

// WarningsTrendSeriesJSON is the trend of one warnings report over the
// health window. Points are newest first.
type WarningsTrendSeriesJSON struct {
	ID     string                   `json:"id"`
	Name   string                   `json:"name"`
	Points []WarningsTrendPointJSON `json:"points"`
}

// QualityTrendJSON is the quality trend of a job over the same runs as
// its health window. It is absent when no run of the window holds a
// quality row.
type QualityTrendJSON struct {
	Coverage []CoverageTrendSeriesJSON `json:"coverage,omitempty"`
	Warnings []WarningsTrendSeriesJSON `json:"warnings,omitempty"`
}

// runQualityMaxReports bounds the coverage and warnings reports one run
// answers. The completion event carries at most this many of each.
const runQualityMaxReports = 20

// roundTenth rounds v to one decimal. It maps a negative zero to zero.
func roundTenth(v float64) float64 {
	r := math.Round(v*10) / 10
	if r == 0 {
		return 0
	}
	return r
}

// qualityPercent answers covered times 100 over total, rounded to one
// decimal. A zero or negative total answers nil.
func qualityPercent(covered, total int32) *float64 {
	if total <= 0 {
		return nil
	}
	p := roundTenth(float64(covered) * 100 / float64(total))
	return &p
}

// counterPercent answers the percent of one counter, or nil when the
// counter is absent.
func counterPercent(c *recordstore.CoverageCounter) *float64 {
	if c == nil {
		return nil
	}
	return qualityPercent(c.Covered, c.Total)
}

// qualityCounter maps one counter, or answers nil when it is absent.
func qualityCounter(c *recordstore.CoverageCounter) *QualityCounterJSON {
	if c == nil {
		return nil
	}
	return &QualityCounterJSON{Covered: c.Covered, Total: c.Total, Percent: qualityPercent(c.Covered, c.Total)}
}

// pointsDelta answers the difference of two percents, each rounded first.
// It answers nil when either counter is absent or has no percent.
func pointsDelta(cur, base *recordstore.CoverageCounter) *float64 {
	pc, pb := counterPercent(cur), counterPercent(base)
	if pc == nil || pb == nil {
		return nil
	}
	d := roundTenth(*pc - *pb)
	return &d
}

// buildRunQuality maps the rows of one run. It answers nil when the run
// holds no row. The rows keep the order of the reader.
func buildRunQuality(q recordstore.RunQuality) *RunQualityJSON {
	if len(q.Coverage) == 0 && len(q.Warnings) == 0 {
		return nil
	}
	out := &RunQualityJSON{}
	for _, row := range q.Coverage {
		if len(out.Coverage) == runQualityMaxReports {
			break
		}
		rep := CoverageReportJSON{
			ID:            row.ReportID,
			Name:          row.Name,
			Line:          qualityCounter(row.Line),
			Branch:        qualityCounter(row.Branch),
			ModifiedLines: qualityCounter(row.ModifiedLines),
			GateStatus:    derefQualityString(row.GateStatus),
		}
		if len(row.Metrics) > 0 {
			rep.Metrics = make(map[string]QualityCounterJSON, len(row.Metrics))
			for name, c := range row.Metrics {
				rep.Metrics[name] = QualityCounterJSON{Covered: c.Covered, Total: c.Total, Percent: qualityPercent(c.Covered, c.Total)}
			}
		}
		if base := row.Base; base != nil {
			rep.Delta = &CoverageDeltaJSON{
				BaseRunID:       base.RunID,
				BaseBuildNumber: base.BuildNumber,
				LinePoints:      pointsDelta(row.Line, base.Line),
				BranchPoints:    pointsDelta(row.Branch, base.Branch),
			}
		}
		out.Coverage = append(out.Coverage, rep)
	}
	for _, row := range q.Warnings {
		if len(out.Warnings) == runQualityMaxReports {
			break
		}
		rep := WarningsReportJSON{
			ID:         row.ReportID,
			Name:       row.Name,
			Total:      row.Total,
			Error:      row.Error,
			High:       row.High,
			Normal:     row.Normal,
			Low:        row.Low,
			New:        row.NewTotal,
			Fixed:      row.FixedTotal,
			GateStatus: derefQualityString(row.GateStatus),
		}
		if base := row.Base; base != nil {
			rep.Delta = &WarningsDeltaJSON{
				BaseRunID:       base.RunID,
				BaseBuildNumber: base.BuildNumber,
				Total:           row.Total - base.Total,
			}
		}
		out.Warnings = append(out.Warnings, rep)
	}
	return out
}

// buildQualityTrend groups the rows of a job's window by report. window is
// the health window, newest first. It sets the build number and the point
// order: points are newest first. A row whose run is not in the window is
// dropped. The series name is the name on the newest point. It answers nil
// when no series remains.
func buildQualityTrend(window []recordstore.HealthRun, q recordstore.RunQuality) *QualityTrendJSON {
	pos := make(map[string]int, len(window))
	num := make(map[string]*int64, len(window))
	for i, run := range window {
		pos[run.RunID] = i
		num[run.RunID] = run.BuildNumber
	}

	covSeries := map[string]*CoverageTrendSeriesJSON{}
	covNewest := map[string]int{}
	for _, row := range q.Coverage {
		i, ok := pos[row.RunID]
		if !ok {
			continue
		}
		s := covSeries[row.ReportID]
		if s == nil {
			s = &CoverageTrendSeriesJSON{ID: row.ReportID}
			covSeries[row.ReportID] = s
			covNewest[row.ReportID] = math.MaxInt
		}
		s.Points = append(s.Points, CoverageTrendPointJSON{
			RunID:         row.RunID,
			BuildNumber:   num[row.RunID],
			LinePercent:   counterPercent(row.Line),
			BranchPercent: counterPercent(row.Branch),
		})
		if i < covNewest[row.ReportID] {
			covNewest[row.ReportID] = i
			s.Name = row.Name
		}
	}
	warnSeries := map[string]*WarningsTrendSeriesJSON{}
	warnNewest := map[string]int{}
	for _, row := range q.Warnings {
		i, ok := pos[row.RunID]
		if !ok {
			continue
		}
		s := warnSeries[row.ReportID]
		if s == nil {
			s = &WarningsTrendSeriesJSON{ID: row.ReportID}
			warnSeries[row.ReportID] = s
			warnNewest[row.ReportID] = math.MaxInt
		}
		s.Points = append(s.Points, WarningsTrendPointJSON{
			RunID:       row.RunID,
			BuildNumber: num[row.RunID],
			Total:       row.Total,
			New:         row.NewTotal,
		})
		if i < warnNewest[row.ReportID] {
			warnNewest[row.ReportID] = i
			s.Name = row.Name
		}
	}

	out := &QualityTrendJSON{}
	for _, s := range covSeries {
		sort.SliceStable(s.Points, func(i, j int) bool { return pos[s.Points[i].RunID] < pos[s.Points[j].RunID] })
		out.Coverage = append(out.Coverage, *s)
	}
	for _, s := range warnSeries {
		sort.SliceStable(s.Points, func(i, j int) bool { return pos[s.Points[i].RunID] < pos[s.Points[j].RunID] })
		out.Warnings = append(out.Warnings, *s)
	}
	if len(out.Coverage) == 0 && len(out.Warnings) == 0 {
		return nil
	}
	sort.Slice(out.Coverage, func(i, j int) bool { return out.Coverage[i].ID < out.Coverage[j].ID })
	sort.Slice(out.Warnings, func(i, j int) bool { return out.Warnings[i].ID < out.Warnings[j].ID })
	return out
}

// runQualityJSON reads the quality rows of one run. A failed read logs a
// warning and answers nil, so the run read still answers without the field.
func (s *Server) runQualityJSON(ctx context.Context, runID string) *RunQualityJSON {
	q, err := s.deps.RecordStore.RunQuality(ctx, runID)
	if err != nil {
		s.deps.Logger.Warn("read run quality failed", "runId", runID, "error", err)
		return nil
	}
	return buildRunQuality(q)
}

// jobQualityTrend reads the quality rows of the runs of a job's health
// window. A failed read logs a warning and answers nil.
func (s *Server) jobQualityTrend(ctx context.Context, cluster, tenant, jobPath string, window []recordstore.HealthRun) *QualityTrendJSON {
	if len(window) == 0 {
		return nil
	}
	ids := make([]string, 0, len(window))
	for _, run := range window {
		ids = append(ids, run.RunID)
	}
	q, err := s.deps.RecordStore.JobQualityTrend(ctx, cluster, tenant, jobPath, ids)
	if err != nil {
		s.deps.Logger.Warn("read job quality trend failed", "jobPath", jobPath, "error", err)
		return nil
	}
	return buildQualityTrend(window, q)
}

// derefQualityString answers the text of a nullable column, or empty.
func derefQualityString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
