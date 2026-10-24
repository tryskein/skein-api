package config

import (
	"reflect"
	"testing"
)

func TestNormalizeCORSOrigins(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"unset", nil, nil, false},
		{"one origin", []string{"https://skein.example.com"}, []string{"https://skein.example.com"}, false},
		{"space after comma", []string{"https://a.example", " http://localhost:8080"}, []string{"https://a.example", "http://localhost:8080"}, false},
		{"trailing comma", []string{"https://a.example", ""}, []string{"https://a.example"}, false},
		{"only blanks", []string{" "}, nil, false},
		{"wildcard", []string{"*"}, []string{"*"}, false},
		{"bare host", []string{"skein.example.com"}, nil, true},
		{"trailing slash", []string{"https://skein.example.com/"}, nil, true},
		{"with path", []string{"https://skein.example.com/app"}, nil, true},
		{"no host", []string{"https://"}, nil, true},
		{"credentials", []string{"https://user@skein.example.com"}, nil, true},
		{"non numeric port", []string{"https://skein.example.com:abc"}, nil, true},
		{"query", []string{"https://skein.example.com?x=1"}, nil, true},
		{"fragment", []string{"https://skein.example.com#x"}, nil, true},
		{"other scheme", []string{"ftp://skein.example.com"}, nil, true},
		{"port", []string{"http://localhost:5173"}, []string{"http://localhost:5173"}, false},
		// the default list of docker-compose.local.yml
		{"local compose defaults", []string{"http://localhost:8080", "http://localhost:5173", "http://host.docker.internal:8080"},
			[]string{"http://localhost:8080", "http://localhost:5173", "http://host.docker.internal:8080"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeCORSOrigins(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
