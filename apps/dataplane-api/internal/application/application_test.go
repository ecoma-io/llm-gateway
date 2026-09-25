package application

import (
	"context"
	"errors"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// fakeUsageFacts is the outbound port as this package's tests see it: the page
// or failure to answer with, and the arguments the use case passed down. The
// arguments are recorded because the use case's whole job on the way in is to
// hand them over untouched.
type fakeUsageFacts struct {
	page  dataplane.Page
	err   error
	after string
	limit int
}

func (f *fakeUsageFacts) ReadUsageEvents(_ context.Context, after string, limit int) (dataplane.Page, error) {
	f.after, f.limit = after, limit
	return f.page, f.err
}

func TestVersionReturnsTheBuildVersion(t *testing.T) {
	app := New("v0.1.0", &fakeUsageFacts{}, &fakeCatalog{}, &fakeProjection{})

	if got, want := app.Version(), "v0.1.0"; got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
}

func TestNewPanicsOnAMissingPort(t *testing.T) {
	tests := []struct {
		name        string
		usage       dataplane.UsageFacts
		catalog     dataplane.Catalog
		projections dataplane.ProjectionDelivery
	}{
		{name: "no usage-facts port", catalog: &fakeCatalog{}, projections: &fakeProjection{}},
		{name: "no catalog port", usage: &fakeUsageFacts{}, projections: &fakeProjection{}},
		{name: "no projection port", usage: &fakeUsageFacts{}, catalog: &fakeCatalog{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("New with %s did not panic", tt.name)
				}
			}()
			New("v0.1.0", tt.usage, tt.catalog, tt.projections)
		})
	}
}

func TestApplicationErrorsPreserveTheirMeaning(t *testing.T) {
	cause := errors.New("the storage implementation failed")
	tests := []struct {
		name        string
		err         *Error
		wantCode    Code
		wantMessage string
		wantCause   error
	}{
		{
			name:        "not found retains a safe public message",
			err:         NotFound("version was not found"),
			wantCode:    CodeNotFound,
			wantMessage: "version was not found",
		},
		{
			name:      "internal retains its cause for server-side inspection",
			err:       Internal(cause),
			wantCode:  CodeInternal,
			wantCause: cause,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Code; got != tt.wantCode {
				t.Errorf("Code = %q, want %q", got, tt.wantCode)
			}
			if got := tt.err.Message; got != tt.wantMessage {
				t.Errorf("Message = %q, want %q", got, tt.wantMessage)
			}
			if tt.wantCause != nil && !errors.Is(tt.err, tt.wantCause) {
				t.Errorf("error does not unwrap to %v", tt.wantCause)
			}
		})
	}
}
