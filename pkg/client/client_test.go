package client

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDecodeError_Envelope(t *testing.T) {
	body := `{"error":"not found"}`
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
	err := DecodeError(resp)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", err.StatusCode)
	}
	if err.Message != "not found" {
		t.Errorf("expected 'not found', got %q", err.Message)
	}
}

func TestDecodeError_Nil(t *testing.T) {
	if err := DecodeError(nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestDecodeError_2xx(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := DecodeError(resp); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestDecodeError_NonJSON(t *testing.T) {
	body := `plain text error`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
	err := DecodeError(resp)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Message != body {
		t.Errorf("expected %q, got %q", body, err.Message)
	}
}

func TestNew_Constructs(t *testing.T) {
	c, err := New("http://localhost:9999", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("client is nil")
	}
}

func TestNew_WithUserAgent(t *testing.T) {
	c, err := New("http://localhost:9999", "token", WithUserAgent("test-agent/1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("client is nil")
	}
}
