package version

import "runtime"

var (
	Version = "dev"
	Commit  = "unknown"
	BuiltAt = "unknown"
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"builtAt"`
	GoOS    string `json:"goos"`
	GoArch  string `json:"goarch"`
}

func Get() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		BuiltAt: BuiltAt,
		GoOS:    runtime.GOOS,
		GoArch:  runtime.GOARCH,
	}
}
