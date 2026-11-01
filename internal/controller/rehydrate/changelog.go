package rehydrate

import "encoding/xml"

// maxChangelogEntries bounds the entries RenderChangelog writes to one
// file, so one changelog0.xml stays small inside the bundle budget
// (design.md D1a). It applies even when a caller hands in more:
// RenderChangelog truncates, it does not assume the caller already
// did.
const maxChangelogEntries = 100

// changelogDocXML is the Varroa changelog document design.md D1a
// defines. It is Varroa's own format: no upstream SCM plugin's
// changelog format is reproduced.
type changelogDocXML struct {
	XMLName xml.Name            `xml:"varroa-changelog"`
	Entries []changelogEntryXML `xml:"entry"`
}

type changelogEntryXML struct {
	CommitID    string `xml:"commitId"`
	Author      string `xml:"author"`
	AuthorEmail string `xml:"authorEmail"`
	Timestamp   int64  `xml:"timestamp"`
	Msg         string `xml:"msg"`
}

// RenderChangelog renders one changelog0.xml document for changes,
// oldest first, as the plugin orders them. The cap keeps the newest
// maxChangelogEntries entries, the tail of changes. It answers (nil, nil)
// for an empty changes: the caller writes no file and the owning
// build.xml carries no <checkouts> element in that case.
//
// Every <entry> carries an empty <authorEmail> and no <path> child.
// No component in the build-event pipeline records a commit author's
// email or the affected file paths (see the Change doc comment), and
// "zero or more path elements" is satisfied by zero.
func RenderChangelog(changes []Change) ([]byte, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	entries := changes
	if len(entries) > maxChangelogEntries {
		entries = entries[len(entries)-maxChangelogEntries:]
	}

	doc := changelogDocXML{}
	for _, c := range entries {
		doc.Entries = append(doc.Entries, changelogEntryXML{
			CommitID:  c.CommitID,
			Author:    c.AuthorID,
			Timestamp: c.Timestamp.UnixMilli(),
			Msg:       c.Message,
		})
	}
	return marshalXML(doc)
}
