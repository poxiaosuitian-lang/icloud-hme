package hme

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestCreateAliasSingleAttemptDoesNotRetryReservation(t *testing.T) {
	var generated, reserved atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hme/generate":
			generated.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"result":{"hme":"fixture@example.test"}}`))
		case "/v1/hme/reserve":
			reserved.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"success":false}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	client, err := NewClient(map[string]string{}, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	client.serviceURL = ts.URL
	if _, err := client.CreateAlias("fixture", 1); err == nil {
		t.Fatal("expected reservation error")
	}
	if generated.Load() != 1 || reserved.Load() != 1 {
		t.Fatalf("unexpected retry: generated=%d reserved=%d", generated.Load(), reserved.Load())
	}
}

func TestValidationURLs(t *testing.T) {
	tests := []struct {
		name string
		host string
		want []string
	}{
		{
			name: "全球账号只用全球端点",
			host: "icloud.com",
			want: []string{"https://setup.icloud.com/setup/ws/1/validate"},
		},
		{
			name: "国区账号回退全球端点",
			host: "icloud.com.cn",
			want: []string{
				"https://setup.icloud.com.cn/setup/ws/1/validate",
				"https://setup.icloud.com/setup/ws/1/validate",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{Host: tt.host}
			if got := client.validationURLs(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("validationURLs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRequestOrigin(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://setup.icloud.com/setup/ws/1/validate", "https://www.icloud.com"},
		{"https://setup.icloud.com.cn/setup/ws/1/validate", "https://www.icloud.com.cn"},
		{"https://p123-maildomainws.icloud.com.cn/v2/hme/list", "https://www.icloud.com.cn"},
		{"https://p123-maildomainws.icloud.com/v2/hme/list", "https://www.icloud.com"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := requestOrigin(tt.url); got != tt.want {
				t.Fatalf("requestOrigin(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
