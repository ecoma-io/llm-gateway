package application

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// fakeCatalog is the catalog port as this package's tests see it: the version
// or failure to answer with, and the name the use case passed down. The name is
// recorded because the use case's whole job on the way in is to hand it over
// untouched — a façade that trimmed, case-folded or special-cased the wildcard
// would be deciding catalog policy, and the assertion is that it did not.
type fakeCatalog struct {
	version dataplane.GroupVersion
	err     error
	name    string
	calls   int
}

func (f *fakeCatalog) CurrentGroupVersion(_ context.Context, groupName string) (dataplane.GroupVersion, error) {
	f.calls++
	f.name = groupName
	if f.err != nil {
		return dataplane.GroupVersion{}, f.err
	}
	return f.version, nil
}

func TestCurrentGroupVersionReturnsThePortAnswerUnchanged(t *testing.T) {
	catalog := &fakeCatalog{version: dataplane.GroupVersion{
		GroupName:      "frontier",
		Version:        3,
		GroupVersionID: "0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d",
	}}
	app := New("v0.1.0", &fakeUsageFacts{}, catalog, &fakeProjection{})

	version, err := app.CurrentGroupVersion(context.Background(), "frontier")
	if err != nil {
		t.Fatalf("CurrentGroupVersion() error = %v", err)
	}
	if version != catalog.version {
		t.Errorf("CurrentGroupVersion() = %+v, want the port's answer unchanged", version)
	}
}

func TestCurrentGroupVersionPassesTheNameThroughUntouched(t *testing.T) {
	// The wildcard is the case that tempts a façade into an opinion: it is the
	// catalog's reserved name and it is also syntax in half the world's
	// routers. Neither fact gives this process a reason to touch it.
	tests := []struct {
		name  string
		group string
	}{
		{name: "an ordinary name", group: "frontier"},
		{name: "the catalog's reserved wildcard name", group: "*"},
		{name: "a name with characters a router would escape", group: "team/model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := &fakeCatalog{}
			app := New("v0.1.0", &fakeUsageFacts{}, catalog, &fakeProjection{})

			if _, err := app.CurrentGroupVersion(context.Background(), tt.group); err != nil {
				t.Fatalf("CurrentGroupVersion(%q) error = %v", tt.group, err)
			}
			if catalog.name != tt.group {
				t.Errorf("the port was asked for %q, want %q unchanged", catalog.name, tt.group)
			}
		})
	}
}

func TestCurrentGroupVersionMapsThePortsFailures(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantCode    Code
		wantMessage string
	}{
		{
			name:        "an unprovisioned group is a not-found the caller acts on",
			err:         fmt.Errorf("%w: the group %q has no version", dataplane.ErrGroupVersionNotFound, "frontier"),
			wantCode:    CodeNotFound,
			wantMessage: "no version of the requested alias group exists",
		},
		{
			name:        "an unreadable data plane is not the caller's fault",
			err:         fmt.Errorf("%w: the management call did not complete", dataplane.ErrUpstreamUnavailable),
			wantCode:    CodeUpstreamUnavailable,
			wantMessage: "the data plane is unavailable",
		},
		{
			name:     "a failure the port does not describe is this process's own",
			err:      errors.New("the adapter returned something the port does not define"),
			wantCode: CodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := &fakeCatalog{err: tt.err}
			app := New("v0.1.0", &fakeUsageFacts{}, catalog, &fakeProjection{})

			_, err := app.CurrentGroupVersion(context.Background(), "frontier")
			mapped, ok := As(err)
			if !ok {
				t.Fatalf("CurrentGroupVersion() error = %v, want an application Error", err)
			}
			if mapped.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", mapped.Code, tt.wantCode)
			}
			if tt.wantMessage != "" && mapped.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", mapped.Message, tt.wantMessage)
			}
			// The cause stays reachable for a caller that needs to distinguish
			// the port's own failures programmatically, and stays out of the
			// public message, which is fixed above.
			if !errors.Is(err, tt.err) {
				t.Errorf("error does not unwrap to the port's cause: %v", err)
			}
			if mapped.Code != CodeInternal && mapped.Message == tt.err.Error() {
				t.Errorf("Message = %q, which is the cause's own text; a public message is written here, not inherited", mapped.Message)
			}
		})
	}
}
