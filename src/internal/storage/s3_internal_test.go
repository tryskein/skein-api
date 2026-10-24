package storage

// White-box unit coverage for the storage package's pure helpers
// (sk-105.6 / spec §3.4). These run WITHOUT a live S3 or Postgres — they
// exercise normalizeEndpoint, the only DB/network-free logic in s3.go.
//
// The bucket start-up path (EnsureBucket, CORS) is covered in
// s3_bucket_test.go against a stub of the S3 HTTP boundary. Presigning and
// object calls need a live storage and are covered by the e2e suite.

import "testing"

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		fallbackSSL bool
		wantHost    string
		wantSecure  bool
		wantErr     bool
	}{
		{
			name:        "bare host:port keeps fallback ssl=false",
			endpoint:    "s3:9000",
			fallbackSSL: false,
			wantHost:    "s3:9000",
			wantSecure:  false,
		},
		{
			name:        "bare host:port keeps fallback ssl=true",
			endpoint:    "s3:9000",
			fallbackSSL: true,
			wantHost:    "s3:9000",
			wantSecure:  true,
		},
		{
			name:       "http URL strips scheme, secure=false",
			endpoint:   "http://s3:9000",
			wantHost:   "s3:9000",
			wantSecure: false,
		},
		{
			name:       "https URL strips scheme, secure=true",
			endpoint:   "https://minio.example.com",
			wantHost:   "minio.example.com",
			wantSecure: true,
		},
		{
			name:        "https URL overrides fallbackSSL=false",
			endpoint:    "https://s3.internal:443",
			fallbackSSL: false,
			wantHost:    "s3.internal:443",
			wantSecure:  true,
		},
		{
			name:        "whitespace trimmed",
			endpoint:    "  s3:9000  ",
			fallbackSSL: false,
			wantHost:    "s3:9000",
			wantSecure:  false,
		},
		{
			name:     "empty endpoint errors",
			endpoint: "",
			wantErr:  true,
		},
		{
			name:     "scheme-only URL with no host errors",
			endpoint: "https://",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, secure, err := normalizeEndpoint(tc.endpoint, tc.fallbackSSL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeEndpoint(%q) err = nil, want error", tc.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeEndpoint(%q) unexpected err: %v", tc.endpoint, err)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if secure != tc.wantSecure {
				t.Errorf("secure = %v, want %v", secure, tc.wantSecure)
			}
		})
	}
}
