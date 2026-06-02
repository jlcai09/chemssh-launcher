package gui

import (
	"errors"
	"net"
	"testing"
)

func TestIsRetriableProxyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"connection refused", errors.New("dial tcp 127.0.0.1:8888: connection refused"), true},
		{"forcibly closed", errors.New("read tcp 127.0.0.1:61163->127.0.0.1:8889: wsarecv: An existing connection was forcibly closed by the remote host"), true},
		{"connection reset", errors.New("read tcp 127.0.0.1:12345->127.0.0.1:8888: connection reset by peer"), true},
		{"wsarecv error", errors.New("wsarecv: connection was aborted"), true},
		{"wsasend error", errors.New("wsasend: connection was aborted"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"EOF error", errors.New("unexpected EOF"), true},
		{"generic error", errors.New("some other error"), false},
		{"timeout error", &timeoutError{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriableProxyError(tt.err)
			if got != tt.want {
				t.Errorf("isRetriableProxyError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// timeoutError implements net.Error for testing
type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

var _ net.Error = (*timeoutError)(nil)
