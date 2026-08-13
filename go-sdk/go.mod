module github.com/nevindra/ix/go-sdk

go 1.26.1

require (
	github.com/google/uuid v1.6.0
	github.com/nevindra/oasis v0.21.0
)

require github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect

// LOCAL DEVELOPMENT ONLY — remove before merge.
// Ticket WC-2 spans this module and oasis; the FileHasher capability and
// GlobResult.Entries are not in a published oasis release yet.
replace github.com/nevindra/oasis => ../../oasis
