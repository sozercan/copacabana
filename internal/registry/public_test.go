package registry

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// Public decides whether a source may be patched into a registry that serves patches anonymously.
func TestPublic(t *testing.T) {
	ctx := context.Background()
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	direct := httptest.NewServer(reg)
	defer direct.Close()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	host := strings.TrimPrefix(direct.URL, "http://")
	if err := New([]string{host}).WriteImage(ctx, host+"/team/app@"+d.String(), img); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		status  int // returned for anonymous manifest requests; 0 serves the image
		public  bool
		wantErr bool
	}{
		{"anonymous read", 0, true, false},
		{"needs credentials", http.StatusUnauthorized, false, false},
		{"forbidden", http.StatusForbidden, false, false},
		{"hidden behind a 404", http.StatusNotFound, false, false},
		{"other failure", http.StatusBadRequest, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 0 && strings.Contains(r.URL.Path, "/manifests/") && r.Header.Get("Authorization") == "" {
					w.WriteHeader(tc.status)
					return
				}
				reg.ServeHTTP(w, r)
			}))
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL, "http://")
			public, err := New([]string{host}).Public(ctx, host+"/team/app@"+d.String())
			if public != tc.public || (err != nil) != tc.wantErr {
				t.Fatalf("Public = %v, %v; want %v, err=%v", public, err, tc.public, tc.wantErr)
			}
		})
	}
}
