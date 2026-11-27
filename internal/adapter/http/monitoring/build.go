package monitoring

import (
	"runtime"
	"runtime/debug"
)

// buildInfo exposes artifact provenance, never build flags, paths or environment.
// Modified is nil when VCS stamping was disabled: unknown is not a clean build.
type buildInfo struct {
	Revision     string `json:"revision,omitempty"`
	RevisionTime string `json:"revision_time,omitempty"`
	Modified     *bool  `json:"modified"`
	GoVersion    string `json:"go_version"`
	Target       string `json:"target"`
}

func currentBuild() buildInfo {
	info, _ := debug.ReadBuildInfo()
	return buildMetadata(info)
}

func buildMetadata(info *debug.BuildInfo) buildInfo {
	out := buildInfo{GoVersion: runtime.Version(), Target: runtime.GOOS + "/" + runtime.GOARCH}
	if info == nil {
		return out
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			out.Revision = setting.Value
		case "vcs.time":
			out.RevisionTime = setting.Value
		case "vcs.modified":
			if setting.Value == "true" || setting.Value == "false" {
				modified := setting.Value == "true"
				out.Modified = &modified
			}
		}
	}
	return out
}
