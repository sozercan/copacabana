package trivy

import "testing"

func TestOSFamily(t *testing.T) {
	for _, tc := range []struct {
		name, report, want string
	}{
		{"alpine", `{"Metadata":{"OS":{"Family":"alpine","Name":"3.18.4"}},"Results":[]}`, "alpine"},
		{"no OS", `{"Metadata":{"ImageID":"sha256:x"},"Results":[]}`, ""},
		{"no metadata", `{"Results":[]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse([]byte(tc.report))
			if err != nil {
				t.Fatal(err)
			}
			if got := r.OSFamily(); got != tc.want {
				t.Fatalf("OSFamily() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --insecure also disables certificate checks, so it's set only for images on listed plain-HTTP registries.
func TestInsecureOnlyForListedRegistries(t *testing.T) {
	s := Scanner{InsecureRegistries: []string{"kind-registry:5000", ""}}
	for image, want := range map[string]bool{
		"kind-registry:5000/library/nginx@sha256:" + sixtyFour:          true,
		"registry.copa-registry.svc.cluster.local:5000/staging/nginx:x": false,
		"nginx:1.25":               false,
		"ghcr.io/org/app:1":        false,
		"kind-registry:5001/app:1": false,
		"not a reference":          false,
	} {
		if got := s.insecure(image); got != want {
			t.Errorf("insecure(%q) = %v, want %v", image, got, want)
		}
	}
}

const sixtyFour = "1111111111111111111111111111111111111111111111111111111111111111"
