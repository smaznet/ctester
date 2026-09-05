package version

// Set via -ldflags at build/release time, e.g.:
//
//	-X github.com/aria/x-tester/internal/version.Version=v1.2.3
var (
	Version = "dev"
	Commit  = ""
)

func String() string {
	if Commit != "" && Version != "dev" {
		return Version + " (" + Commit + ")"
	}
	if Version != "" {
		return Version
	}
	return "dev"
}
