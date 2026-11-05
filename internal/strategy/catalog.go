package strategy

import "context"

// Catalog is the host management API. Agents only receive scoped tool
// adapters; they are never given authority to register trusted expectations.
// Runtime execution continues to depend on the smaller Store interface.
type Catalog interface {
	Store
	Save(context.Context, Spec) (string, error)
	SaveSuite(context.Context, Suite, bool) (string, error)
	Suite(context.Context, string) (Suite, error)
	SaveReport(context.Context, Report) (string, error)
	Report(context.Context, string) (Report, error)
	Promote(context.Context, string, bool) error
}
