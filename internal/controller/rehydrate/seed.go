package rehydrate

import "fmt"

// JobRecords is one job path's answer from the record store: its
// current next build number and the completed records it holds,
// newest first. It mirrors recordstore.RehydrationJobRecords by name.
type JobRecords struct {
	JobPath         string
	NextBuildNumber int64
	// Records holds every completed record the caller has for this
	// job, newest first. BuildJobSeed does its own N-plus-newest-
	// success selection over this slice; the caller need not
	// pre-trim it.
	Records []Record
}

// CandidateFile is one raw (uncompressed) file a job seed or a bundle
// may carry: a build.xml, a changelog0.xml, or a nextBuildNumber file.
type CandidateFile struct {
	Path    string
	Content []byte
}

// jobSeedRecord is one selected record of a JobSeed, paired with its
// rendered files.
type jobSeedRecord struct {
	buildNumber int64
	// protected is true for the seed's newest record and its newest
	// successful record: the budget pass (design.md D4, order 3) SHALL
	// NOT drop a protected record.
	protected bool
	buildXML  CandidateFile
	changelog *CandidateFile // nil when the record carries no change
}

// JobSeed is the operator's per-job selection: the job's
// nextBuildNumber file plus the selected records' rendered files, in
// the mite's allow-listed path shape (design.md D3/D5).
type JobSeed struct {
	JobPath    string
	NumberFile CandidateFile
	records    []jobSeedRecord // newest first
}

// RecordCount answers how many records this seed carries before any
// budget trim (task 6.1's per-tenant Records accounting reads this
// immediately after BuildJobSeed, ahead of SelectForBundle's own
// cross-tenant trim).
func (s JobSeed) RecordCount() int {
	return len(s.records)
}

// BuildJobSeed selects jr's job seed and renders every file it needs:
// the nextBuildNumber file, the N newest records plus the newest
// successful record (design.md D4), and each selected record's
// build.xml and, when it carries a change, its changelog0.xml. n is
// Team.spec.pipelines.rehydrateBuilds. jenkinsHome is the cell's
// Jenkins container's JENKINS_HOME, used to build each <checkouts>
// changelogFile path.
func BuildJobSeed(jr JobRecords, n int, jenkinsHome string, opts Options) (JobSeed, error) {
	selected, protected := selectRecords(jr.Records, n)

	seed := JobSeed{
		JobPath: jr.JobPath,
		NumberFile: CandidateFile{
			Path:    NextBuildNumberPath(jr.JobPath),
			Content: []byte(formatNextBuildNumber(jr.NextBuildNumber)),
		},
	}

	for i, rec := range selected {
		var changelogAbsPath string
		var changelog *CandidateFile
		if len(rec.Changes) > 0 {
			changelogAbsPath = ChangelogAbsolutePath(jenkinsHome, jr.JobPath, rec.BuildNumber)
			body, err := RenderChangelog(rec.Changes)
			if err != nil {
				return JobSeed{}, fmt.Errorf("render changelog for %s build %d: %w", jr.JobPath, rec.BuildNumber, err)
			}
			changelog = &CandidateFile{Path: ChangelogFilePath(jr.JobPath, rec.BuildNumber), Content: body}
		}

		buildXMLBody, err := RenderBuildXML(rec, opts, changelogAbsPath)
		if err != nil {
			return JobSeed{}, fmt.Errorf("render build.xml for %s build %d: %w", jr.JobPath, rec.BuildNumber, err)
		}

		seed.records = append(seed.records, jobSeedRecord{
			buildNumber: rec.BuildNumber,
			protected:   protected[i],
			buildXML:    CandidateFile{Path: BuildXMLPath(jr.JobPath, rec.BuildNumber), Content: buildXMLBody},
			changelog:   changelog,
		})
	}

	return seed, nil
}

// selectRecords answers the N-newest-plus-newest-successful selection
// design.md D4 defines, and which of the answer's entries are
// protected from the budget pass's order-3 drop. all is newest first.
// n below zero is treated as zero.
//
// The newest successful record is always included, for every n
// including 0; only the further records are gated on n above zero.
func selectRecords(all []Record, n int) (selected []Record, protected []bool) {
	if n < 0 {
		n = 0
	}
	limit := n
	if limit > len(all) {
		limit = len(all)
	}

	successIdx := -1
	for i, r := range all {
		if r.Result == resultSuccess {
			successIdx = i
			break
		}
	}

	include := make([]bool, len(all))
	for i := 0; i < limit; i++ {
		include[i] = true
	}
	if successIdx >= 0 {
		include[successIdx] = true
	}

	for i, inc := range include {
		if inc {
			selected = append(selected, all[i])
		}
	}

	protected = make([]bool, len(selected))
	if len(protected) > 0 {
		protected[0] = true
	}
	if successIdx >= 0 {
		pos := 0
		for i := 0; i < successIdx; i++ {
			if include[i] {
				pos++
			}
		}
		protected[pos] = true
	}

	return selected, protected
}

// formatNextBuildNumber answers the nextBuildNumber file's content:
// the decimal value followed by a newline, Jenkins's own format.
func formatNextBuildNumber(n int64) string {
	return fmt.Sprintf("%d\n", n)
}
