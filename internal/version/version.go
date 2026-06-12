// Package version exposes the binary's build metadata, stamped by the
// release pipeline via -ldflags (see .goreleaser.yaml).
package version

// Defaults describe a from-source build; releases overwrite them.
var (
	// Version is the semantic version, "dev" when not stamped.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = ""
	// Date is the build timestamp.
	Date = ""
)

// String renders the version for --version and, later, the admin API.
func String() string {
	s := Version
	if Commit != "" {
		s += " (" + Commit
		if Date != "" {
			s += ", " + Date
		}
		s += ")"
	}
	return s
}
