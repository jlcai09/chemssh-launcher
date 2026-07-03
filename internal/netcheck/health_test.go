package netcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitForURLTreatsHTTP499AsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(499)
	}))
	defer server.Close()

	if err := WaitForURL(context.Background(), server.URL, time.Second, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForURLWithOptionsRejectsConfiguredStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	err := WaitForURLWithOptions(context.Background(), server.URL, 50*time.Millisecond, 10*time.Millisecond, HealthOptions{
		RejectStatuses: map[int]string{http.StatusUnauthorized: "bad token"},
	})
	if err == nil {
		t.Fatal("expected health check to reject configured status")
	}
}

func TestWaitForURLFailsWhenServerErrorPersists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if err := WaitForURL(context.Background(), server.URL, 50*time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("expected health check to fail")
	}
}
