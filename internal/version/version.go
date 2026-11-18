package version

var (
	ver    = "dev"
	commit = "none"
	date   = "unknown"
)

func Version() string { return ver }

// Commit returns the build commit.
func Commit() string { return commit }

// Date returns the build date.
func Date() string { return date }
