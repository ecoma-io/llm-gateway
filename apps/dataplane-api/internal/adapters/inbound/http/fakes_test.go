package http

import (
	"context"
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// testCredential is the shared secret the handler tests configure. It is
// distinctive on purpose: several assertions below check that it appears in no
// response body, no error and no log line, and a value like "secret" would make
// those checks fire on a coincidence.
const testCredential = "test-service-credential-2f8c41"

// usageCall records one read the handler made of the port, so that "nothing
// reached the Data Plane" is an assertion rather than an assumption. A refusal
// test that only checked the status would pass on a handler that authenticated
// the caller *after* consulting the feed.
type usageCall struct {
	after string
	limit int
}

// fakeUsageFacts is the outbound port as the handler tests see it: a page to
// answer with, or a failure to answer with instead, plus the record of every
// call. It is a hand-written double rather than a mocking library, following
// the repository's rule that the first test double is no reason to take a
// dependency.
type fakeUsageFacts struct {
	page  dataplane.Page
	err   error
	calls []usageCall
}

func (f *fakeUsageFacts) ReadUsageEvents(_ context.Context, after string, limit int) (dataplane.Page, error) {
	f.calls = append(f.calls, usageCall{after: after, limit: limit})
	if f.err != nil {
		return dataplane.Page{}, f.err
	}
	return f.page, nil
}

// called reports whether anything reached the port.
func (f *fakeUsageFacts) called() bool { return len(f.calls) > 0 }

// fakeCatalog is the catalog port as the handler tests see it: one version to
// answer with, or a failure to answer with instead, plus the record of every
// name it was asked for. It is the same kind of hand-written double as
// fakeUsageFacts, and the recorded names are what make the wildcard row in
// aliasgroups_test.go an assertion about what crossed rather than a status
// that happened to be green.
type fakeCatalog struct {
	version dataplane.GroupVersion
	err     error
	names   []string
}

func (f *fakeCatalog) CurrentGroupVersion(_ context.Context, groupName string) (dataplane.GroupVersion, error) {
	f.names = append(f.names, groupName)
	if f.err != nil {
		return dataplane.GroupVersion{}, f.err
	}
	return f.version, nil
}

// called reports whether anything reached the port.
func (f *fakeCatalog) called() bool { return len(f.names) > 0 }

// testApp is the application the route-table tests read a surface from. The
// ports are fakes because those tests serve no request — they assert the table
// as data, and the handler behaviour is driven through the tests in
// usageevents_test.go and aliasgroups_test.go.
func testApp() *application.App {
	return application.New("test", &fakeUsageFacts{}, &fakeCatalog{})
}

// testHandler returns the real handler over app and the real authenticator
// configured with testCredential. The authenticator is the production one, not
// a double: the check it performs is the behaviour under test, and a double
// would let a broken comparison pass.
func testHandler(app *application.App) stdhttp.Handler {
	return New(app, NewServiceAuthenticator(testCredential))
}
