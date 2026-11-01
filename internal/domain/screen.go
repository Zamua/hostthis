package domain

import "io"

// ScreenShape names what a screening request carries (docs/SPEC.md
// "Pre-publish screening").
type ScreenShape string

const (
	// ScreenDocument is one paste file's decoded bytes.
	ScreenDocument ScreenShape = "document"
	// ScreenArchive is a gzip tar bundle of a site's files.
	ScreenArchive ScreenShape = "archive"
)

// ScreenRequest is one upload offered for screening.
type ScreenRequest struct {
	Shape ScreenShape
	Name  string
	// Truncated marks a document cut to its first ScreenDocumentMax bytes.
	Truncated bool
	Body      io.Reader
}

// ScreenVerdict is the screening service's answer.
type ScreenVerdict struct {
	Allow bool
	Rule  string
	File  string
}

// ScreenDocumentMax bounds the document bytes sent for screening.
const ScreenDocumentMax = 12 << 20
