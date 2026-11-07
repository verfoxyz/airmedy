package config

const (
	AppName    = "Airmedy"
	AppDesc    = "A lightweight offline music player for macOS, Windows and Linux."
	Version    = "1.3.3-plugins-alpha.2"
	RepoOwner  = "misa198"
	RepoName   = "airmedy"
	GitHubURL  = "https://github.com/" + RepoOwner + "/" + RepoName
	LicenseURL = GitHubURL + "/blob/master/LICENSE"
)

// UpdatesDisabled marks builds that must never talk to the release feed. The
// plugins alpha ships without in-app updates: it shares the official update
// channel, and accepting an update would silently replace the fork.
const UpdatesDisabled = true
