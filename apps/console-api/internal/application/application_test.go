package application

import (
	"errors"
	"testing"
)

func TestVersionReturnsTheBuildVersion(t *testing.T) {
	app := New("v0.1.0")

	if got, want := app.Version(), "v0.1.0"; got != want {
		t.Errorf("Version() = %q, want %q", got, want)
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
