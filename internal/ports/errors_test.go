package ports_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/ports"
)

func TestStatusErrorIsTransientOnlyForServerErrors(t *testing.T) {
	for code, want := range map[int]bool{400: false, 404: false, 409: false, 429: false, 500: true, 503: true} {
		err := fmt.Errorf("call: %w", &ports.StatusError{Service: "erp", StatusCode: code})
		var marked interface{ Transient() bool }
		if !errors.As(err, &marked) || marked.Transient() != want {
			t.Errorf("status %d: transient = %v, want %v", code, marked != nil && marked.Transient(), want)
		}
	}
}

func TestStatusErrorMessageNamesServiceAndCode(t *testing.T) {
	got := (&ports.StatusError{Service: "erp", StatusCode: 503}).Error()
	if got != "erp responded with status 503" {
		t.Fatalf("message = %q", got)
	}
}
