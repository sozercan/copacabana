package registry

import (
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestPlatformString(t *testing.T) {
	for _, tc := range []struct {
		p    v1.Platform
		want string
	}{
		{v1.Platform{OS: "linux", Architecture: "amd64"}, "linux/amd64"},
		{v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, "linux/arm64"},
		{v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, "linux/arm/v7"},
	} {
		if got := PlatformString(&tc.p); got != tc.want {
			t.Errorf("PlatformString(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}
