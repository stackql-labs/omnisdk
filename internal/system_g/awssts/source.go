// Package awssts implements the AWS STS credential protocol. It owns acquisition,
// expiry and coalescing; it knows nothing about queries, plans, ledgers or sessions.
package awssts

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/awsv4"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

// Config is the explicit STS request contract. Region never comes from downstream scope.
type Config struct {
	RoleARN         string
	ExternalID      string
	SessionName     string
	DurationSeconds int32
	Region          string
	Endpoint        string
}

type flight struct {
	done        chan struct{}
	cancel      context.CancelFunc
	waiters     int
	credentials facade.AWSCredentials
	err         error
}
type source struct {
	config      Config
	base        facade.AWSCredentialSource
	now         func() time.Time
	mu          sync.Mutex
	credentials facade.AWSCredentials
	refreshAt   time.Time
	flight      *flight
}

var sessionName = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{2,64}$`)
var roleARN = regexp.MustCompile(`^arn:[a-z0-9-]+:iam::[0-9]{12}:role/.+$`)
var errorCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

// New configures a source without network IO. now is injectable for expiry tests.
func New(config Config, base facade.AWSCredentialSource, now func() time.Time) (facade.AWSCredentialSource, error) {
	if base == nil {
		return nil, fmt.Errorf("STS source credentials are required")
	}
	if !roleARN.MatchString(config.RoleARN) {
		return nil, fmt.Errorf("STS role ARN must identify an IAM role")
	}
	if !sessionName.MatchString(config.SessionName) {
		return nil, fmt.Errorf("STS session name must contain 2-64 permitted characters")
	}
	if config.Region == "" || strings.ContainsAny(config.Region, "/\\ :?#\r\n\t") {
		return nil, fmt.Errorf("STS region must be explicit")
	}
	if config.DurationSeconds != 0 && (config.DurationSeconds < 900 || config.DurationSeconds > 43200) {
		return nil, fmt.Errorf("STS duration must be between 900 and 43200 seconds")
	}
	if config.Endpoint == "" {
		suffix := "amazonaws.com"
		if strings.HasPrefix(config.Region, "cn-") {
			suffix = "amazonaws.com.cn"
		}
		config.Endpoint = "https://sts." + config.Region + "." + suffix
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, fmt.Errorf("STS endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	if now == nil {
		now = time.Now
	}
	return &source{config: config, base: base, now: now}, nil
}

func (s *source) Retrieve(ctx context.Context) (facade.AWSCredentials, error) {
	if err := ctx.Err(); err != nil {
		return facade.AWSCredentials{}, err
	}
	s.mu.Lock()
	if !s.credentials.Expiration.IsZero() && s.now().Before(s.refreshAt) {
		value := s.credentials
		s.mu.Unlock()
		return value, nil
	}
	f := s.flight
	if f == nil {
		// One canceled waiter must not cancel the acquisition of remaining callers.
		acquisition, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		f = &flight{done: make(chan struct{}), cancel: cancel}
		s.flight = f
		go s.acquire(acquisition, f)
	}
	f.waiters++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		f.waiters--
		if f.waiters == 0 && s.flight == f {
			s.flight = nil
			f.cancel()
		}
		s.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return facade.AWSCredentials{}, ctx.Err()
	case <-f.done:
		if err := ctx.Err(); err != nil {
			return facade.AWSCredentials{}, err
		}
		return f.credentials, f.err
	}
}
func (s *source) acquire(ctx context.Context, f *flight) {
	defer f.cancel()
	credentials, err := s.request(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	f.credentials, f.err = credentials, err
	if s.flight == f {
		s.flight = nil
		if err == nil {
			s.credentials = credentials
			margin := min(time.Minute, credentials.Expiration.Sub(s.now())/10)
			s.refreshAt = credentials.Expiration.Add(-margin)
		}
	}
	close(f.done)
}

type transportError struct{ cause error }

func (e transportError) Error() string { return "STS AssumeRole request failed" }
func (e transportError) Unwrap() error { return e.cause }

func (s *source) request(ctx context.Context) (facade.AWSCredentials, error) {
	credentials, err := s.base.Retrieve(ctx)
	if err != nil {
		return facade.AWSCredentials{}, err
	}
	values := url.Values{"Action": {"AssumeRole"}, "Version": {"2011-06-15"}, "RoleArn": {s.config.RoleARN}, "RoleSessionName": {s.config.SessionName}}
	if s.config.ExternalID != "" {
		values.Set("ExternalId", s.config.ExternalID)
	}
	if s.config.DurationSeconds != 0 {
		values.Set("DurationSeconds", strconv.FormatInt(int64(s.config.DurationSeconds), 10))
	}
	payload := []byte(values.Encode())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.Endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return facade.AWSCredentials{}, fmt.Errorf("invalid STS request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	signer := awsv4.NewSigV4Signer(s.config.Region, "sts", awsv4.Credentials{AccessKeyID: credentials.AccessKeyID, SecretAccessKey: credentials.SecretAccessKey, SessionToken: credentials.SessionToken}, false)
	if err := signer.Sign(request, payload, s.now()); err != nil {
		return facade.AWSCredentials{}, err
	}
	if err := facade.CheckTransport(ctx, "transport/http/"+request.URL.Host, request.Method); err != nil {
		return facade.AWSCredentials{}, err
	}
	client := *httpx.ClientFrom(ctx, http.DefaultClient)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return facade.AWSCredentials{}, transportError{cause: err}
	}
	defer response.Body.Close()
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		return facade.AWSCredentials{}, fmt.Errorf("STS response unreadable or exceeds size limit")
	}
	if response.StatusCode != http.StatusOK {
		var failure struct{ Error struct{ Code string } }
		_ = xml.Unmarshal(body, &failure)
		code := "unknown"
		if errorCode.MatchString(failure.Error.Code) {
			code = failure.Error.Code
		}
		return facade.AWSCredentials{}, fmt.Errorf("STS AssumeRole rejected: status %d, code %s", response.StatusCode, code)
	}
	var result struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string
				SessionToken    string
				Expiration      string
			}
		} `xml:"AssumeRoleResult"`
	}
	if xml.Unmarshal(body, &result) != nil {
		return facade.AWSCredentials{}, fmt.Errorf("STS returned malformed credential XML")
	}
	raw := result.Result.Credentials
	expiration, err := time.Parse(time.RFC3339, raw.Expiration)
	if err != nil || !expiration.After(s.now()) || raw.AccessKeyID == "" || raw.SecretAccessKey == "" || raw.SessionToken == "" {
		return facade.AWSCredentials{}, fmt.Errorf("STS returned incomplete or expired credentials")
	}
	return facade.AWSCredentials{AccessKeyID: raw.AccessKeyID, SecretAccessKey: raw.SecretAccessKey, SessionToken: raw.SessionToken, Expiration: expiration}, nil
}
