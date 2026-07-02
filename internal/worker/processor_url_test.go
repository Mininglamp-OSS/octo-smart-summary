package worker

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

func TestValidateOctoSearchURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "cluster http", raw: "http://octo-search-batch:8080"},
		{name: "cluster https", raw: "https://octo-search-batch:8443"},
		{name: "trailing slash", raw: "http://octo-search-batch:8080/"},
		{name: "missing scheme", raw: "octo-search-batch:8080", wantErr: true},
		{name: "missing host", raw: "http://", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://octo-search-batch:8080", wantErr: true},
		{name: "with v1 path", raw: "http://octo-search-batch:8080/v1", wantErr: true},
		{name: "with nested v1 path", raw: "http://octo-search-batch:8080/api/v1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateOctoSearchURL(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateOctoSearchURL(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestNewOctoSearchClientForBackend(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.Config
		wantNil bool
		wantErr bool
	}{
		{
			name:    "batch creates client",
			cfg:     &config.Config{MessageFetchBackend: "batch", OctoSearchURL: "http://octo-search-batch:8080", OctoSearchToken: "tok"},
			wantNil: false,
		},
		{
			name:    "batch requires url and token",
			cfg:     &config.Config{MessageFetchBackend: "batch"},
			wantNil: true,
			wantErr: true,
		},
		{
			name:    "mysql does not need octo config",
			cfg:     &config.Config{MessageFetchBackend: "mysql"},
			wantNil: true,
		},
		{
			name:    "unknown backend",
			cfg:     &config.Config{MessageFetchBackend: "other"},
			wantNil: true,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := newOctoSearchClientForBackend(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if (client == nil) != tt.wantNil {
				t.Fatalf("client nil = %v, wantNil %v", client == nil, tt.wantNil)
			}
		})
	}
}
