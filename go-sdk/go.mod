module github.com/nevindra/ix/go-sdk

go 1.26.1

require (
	github.com/google/uuid v1.6.0
	github.com/nevindra/oasis v0.17.0
)

// TODO: remove before release — replace with a published oasis version once
// the Browser-capability changes (CreateOpts.Browser, WithoutBrowser) are tagged.
replace github.com/nevindra/oasis => /home/nezhifi/Development/oasis
