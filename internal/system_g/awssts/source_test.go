package awssts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type baseSource struct{}

func (baseSource) Retrieve(ctx context.Context) (facade.AWSCredentials, error) {
	return facade.AWSCredentials{AccessKeyID: "BASEKEY", SecretAccessKey: "BASESECRET", SessionToken: "BASETOKEN"}, ctx.Err()
}
func fixtureConfig(endpoint string) Config {
	return Config{RoleARN: "arn:aws:iam::123456789012:role/fixture", ExternalID: "external", SessionName: "fixture-session", DurationSeconds: 900, Region: "us-east-1", Endpoint: endpoint}
}
func credentialXML(key string, expiration time.Time) string {
	return fmt.Sprintf(`<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>SECRET</SecretAccessKey><SessionToken>TOKEN</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`, key, expiration.UTC().Format(time.RFC3339Nano))
}
func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture did not reach expected checkpoint")
	}
}
func waiters(t *testing.T, s facade.AWSCredentialSource, want int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		live := s.(*source)
		live.mu.Lock()
		count := 0
		if live.flight != nil {
			count = live.flight.waiters
		}
		live.mu.Unlock()
		if count == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("waiters=%d want=%d", count, want)
		case <-tick.C:
		}
	}
}

func TestConcurrentAcquisitionAndExpiryRefresh(t *testing.T) {
	var clockMu sync.Mutex
	clock := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		generation := calls.Add(1)
		if r.Method != "POST" || r.Header.Get("X-Amz-Security-Token") != "BASETOKEN" || !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/sts/aws4_request") {
			t.Error("source request not signed correctly")
		}
		_ = r.ParseForm()
		if r.Form.Get("RoleArn") != fixtureConfig("").RoleARN || r.Form.Get("ExternalId") != "external" || r.Form.Get("RoleSessionName") != "fixture-session" || r.Form.Get("DurationSeconds") != "900" {
			t.Error("AssumeRole parameters lost")
		}
		if generation == 1 {
			close(started)
			<-release
		}
		fmt.Fprint(w, credentialXML(fmt.Sprintf("ROLE%d", generation), now().Add(time.Hour)))
	}))
	defer server.Close()
	credentials, err := New(fixtureConfig(server.URL), baseSource{}, now)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 12)
	for range 12 {
		go func() {
			value, err := credentials.Retrieve(context.Background())
			if err == nil && value.AccessKeyID != "ROLE1" {
				err = fmt.Errorf("wrong shared generation %s", value.AccessKeyID)
			}
			results <- err
		}()
	}
	await(t, started)
	waiters(t, credentials, 12)
	close(release)
	for range 12 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("acquisition stampede", calls.Load())
	}
	for generation := 2; generation <= 3; generation++ {
		clockMu.Lock()
		clock = clock.Add(time.Hour)
		clockMu.Unlock()
		value, err := credentials.Retrieve(context.Background())
		if err != nil || value.AccessKeyID != fmt.Sprintf("ROLE%d", generation) {
			t.Fatal(value.AccessKeyID, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("expired credentials reused", calls.Load())
	}
}

func TestCanceledWaiterDoesNotCancelOtherCallers(t *testing.T) {
	started, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			close(canceled)
			return
		}
		fmt.Fprint(w, credentialXML("ROLE", time.Now().Add(time.Hour)))
	}))
	defer server.Close()
	credentials, err := New(fixtureConfig(server.URL), baseSource{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcomes := make(chan error, 2)
	go func() { _, err := credentials.Retrieve(first); outcomes <- err }()
	await(t, started)
	go func() { _, err := credentials.Retrieve(context.Background()); outcomes <- err }()
	waiters(t, credentials, 2)
	cancel()
	if err := <-outcomes; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-canceled:
		t.Fatal("one waiter canceled shared request")
	default:
	}
	close(release)
	if err := <-outcomes; err != nil {
		t.Fatal(err)
	}
}

func TestLastCanceledWaiterAbortsRequestAndAllowsRetry(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if requests.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(canceled)
			return
		}
		fmt.Fprint(w, credentialXML("REPLACEMENT", time.Now().Add(time.Hour)))
	}))
	defer server.Close()
	credentials, err := New(fixtureConfig(server.URL), baseSource{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := credentials.Retrieve(ctx); result <- err }()
	await(t, started)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	await(t, canceled)
	value, err := credentials.Retrieve(context.Background())
	if err != nil || value.AccessKeyID != "REPLACEMENT" {
		t.Fatal(value.AccessKeyID, err)
	}
}

func TestFailedAcquisitionIsSanitizedAndRetryable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if calls.Add(1) == 1 {
			w.WriteHeader(403)
			fmt.Fprint(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>BASESECRET external</Message></Error></ErrorResponse>`)
			return
		}
		fmt.Fprint(w, credentialXML("RETRY", time.Now().Add(time.Hour)))
	}))
	defer server.Close()
	credentials, err := New(fixtureConfig(server.URL), baseSource{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Retrieve(context.Background()); err == nil || strings.Contains(err.Error(), "BASESECRET") || strings.Contains(err.Error(), "external") {
		t.Fatal("failure leaked or was ignored", err)
	}
	if value, err := credentials.Retrieve(context.Background()); err != nil || value.AccessKeyID != "RETRY" {
		t.Fatal(value.AccessKeyID, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := credentials.Retrieve(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("cached result ignored cancellation", err)
	}
}

func TestInvalidResponsesNeverBecomeCachedCredentials(t *testing.T) {
	for _, body := range []string{`not xml`, `<AssumeRoleResponse/>`, credentialXML("OLD", time.Now().Add(-time.Hour))} {
		t.Run(fmt.Sprintf("bytes-%d", len(body)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			credentials, err := New(fixtureConfig(server.URL), baseSource{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := credentials.Retrieve(context.Background()); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestSourceCacheSeparatesScopesAndCredentialGenerations(t *testing.T) {
	cache := NewCache()
	var built int
	factory := func() (facade.AWSCredentialSource, error) {
		built++
		return New(fixtureConfig("http://localhost/sts"), baseSource{}, nil)
	}
	first, err := cache.Source("tenant-a", [32]byte{1}, factory)
	if err != nil {
		t.Fatal(err)
	}
	same, err := cache.Source("tenant-a", [32]byte{1}, factory)
	if err != nil || same != first || built != 1 {
		t.Fatal("source not reused")
	}
	rotated, err := cache.Source("tenant-a", [32]byte{2}, factory)
	if err != nil || rotated == first {
		t.Fatal("rotation reused prior source")
	}
	other, err := cache.Source("tenant-b", [32]byte{2}, factory)
	if err != nil || other == rotated {
		t.Fatal("tenancies shared credential source")
	}
}
