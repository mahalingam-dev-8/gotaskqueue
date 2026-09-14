package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/queue"
)

func TestRegistryLookup(t *testing.T) {
	r := NewRegistry()
	r.RegisterFunc("email:send", func(context.Context, queue.Job) error { return nil })

	if _, err := r.get("email:send"); err != nil {
		t.Fatalf("get registered handler: %v", err)
	}

	_, err := r.get("does:not:exist")
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
	// A missing handler must never be retried.
	if !IsPermanent(err) {
		t.Error("ErrNoHandler should be permanent")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	// defer + recover is how you assert on a panic in a test.
	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate job type should panic")
		}
	}()

	r := NewRegistry()
	r.RegisterFunc("dup", func(context.Context, queue.Job) error { return nil })
	r.RegisterFunc("dup", func(context.Context, queue.Job) error { return nil })
}

func TestPermanentErrorSurvivesWrapping(t *testing.T) {
	base := errors.New("bad payload")
	wrapped := fmt.Errorf("handling job: %w", Permanent(base))

	if !IsPermanent(wrapped) {
		t.Error("IsPermanent should see through fmt.Errorf wrapping")
	}
	if !errors.Is(wrapped, base) {
		t.Error("Permanent should not hide the underlying error")
	}
	if IsPermanent(errors.New("transient")) {
		t.Error("a plain error must not be permanent")
	}
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) should stay nil")
	}
}
