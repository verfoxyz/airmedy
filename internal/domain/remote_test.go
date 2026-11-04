package domain

import "testing"

func TestRemotePath_NormalisesProviderAndTrimsID(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		externalID string
		want       string
	}{
		{"plain", "wy", "509781655", "remote://wy/509781655"},
		{"provider upper", "WY", "509781655", "remote://wy/509781655"},
		{"provider padded", "  tx  ", "123", "remote://tx/123"},
		{"id padded", "wy", "  abc  ", "remote://wy/abc"},
		{"id keeps case", "wy", "AbC123", "remote://wy/AbC123"},
		{"empty", "", "", "remote:///"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RemotePath(tt.provider, tt.externalID); got != tt.want {
				t.Fatalf("RemotePath(%q, %q) = %q, want %q", tt.provider, tt.externalID, got, tt.want)
			}
		})
	}
}

func TestRemotePath_IsStableForSameIdentity(t *testing.T) {
	// The UNIQUE(path) constraint is what deduplicates a track across sources,
	// so the same identity must always produce the identical string.
	a := RemotePath("wy", "509781655")
	b := RemotePath(" WY ", "509781655 ")
	if a != b {
		t.Fatalf("RemotePath is not stable: %q != %q", a, b)
	}
}

func TestTrack_IsRemote(t *testing.T) {
	tests := []struct {
		name string
		t    *Track
		want bool
	}{
		{"nil", nil, false},
		{"local ready", &Track{Availability: AvailabilityReady, Path: `/music/a.mp3`}, false},
		{"legacy row without availability", &Track{Path: `/music/a.mp3`}, false},
		{"remote", &Track{Availability: AvailabilityRemote, Path: RemotePath("wy", "1")}, true},
		{"downloading", &Track{Availability: AvailabilityDownloading, Path: RemotePath("wy", "1")}, true},
		{"failed", &Track{Availability: AvailabilityFailed, Path: RemotePath("wy", "1")}, true},
		{"ready but virtual path", &Track{Availability: AvailabilityReady, Path: RemotePath("wy", "1")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.t.IsRemote(); got != tt.want {
				t.Fatalf("IsRemote() = %v, want %v", got, tt.want)
			}
		})
	}
}
