package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeURL(t *testing.T) {
	tests := []struct {
		addr    string
		want    string
		wantErr bool
	}{
		{addr: ":8080", want: "http://127.0.0.1:8080/readyz"},
		{addr: "0.0.0.0:8080", want: "http://127.0.0.1:8080/readyz"},
		{addr: "127.0.0.1:9090", want: "http://127.0.0.1:9090/readyz"},
		{addr: "[::]:8080", want: "http://127.0.0.1:8080/readyz"},
		{addr: "8080", want: "http://127.0.0.1:8080/readyz"},
		{addr: " :8080 ", want: "http://127.0.0.1:8080/readyz"},
		{addr: "", wantErr: true},
		{addr: "localhost", wantErr: true},
		{addr: ":http", wantErr: true},
		{addr: ":0", wantErr: true},
		{addr: ":70000", wantErr: true},
	}
	for _, tc := range tests {
		got, err := probeURL(tc.addr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("probeURL(%q) = %q, want error", tc.addr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("probeURL(%q): %v", tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("probeURL(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestProbeAPI(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "ready", statusCode: http.StatusOK},
		{name: "not ready", statusCode: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/readyz" {
					t.Errorf("path = %q, want /readyz", r.URL.Path)
				}
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			addr := strings.TrimPrefix(srv.URL, "http://")
			err := probeAPI(context.Background(), addr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("probeAPI() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
