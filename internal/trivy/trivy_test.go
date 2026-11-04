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
