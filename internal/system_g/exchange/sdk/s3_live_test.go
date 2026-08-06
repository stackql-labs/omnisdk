package sdk

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

// TestS3ListBucketsLive hits real S3, skipped unless AWS credentials are in the env.
func TestS3ListBucketsLive(t *testing.T) {
	ak := os.Getenv("AWS_ACCESS_KEY_ID")
	sk := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if ak == "" || sk == "" {
		t.Skip("no AWS credentials in env; skipping live S3 test")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	creds := Credentials{
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
	ex := NewS3ListBuckets(1, region, creds, "", 1000) // all pages, real AWS

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rs := ex.Open(ctx)
	defer rs.Close()

	pages := 0
	for rs.Next(ctx) {
		pages++
		rec := rs.Record()
		status := readValue(t, rec, httpx.KeyStatus)
		body := readValue(t, rec, facade.AnonymousPayload)
		if status != "200" {
			t.Fatalf("page %d status = %s, body:\n%s", pages, status, body)
		}
		if !strings.Contains(body, "<ListAllMyBucketsResult") {
			t.Errorf("page %d has no ListAllMyBucketsResult:\n%s", pages, body)
		}
		t.Logf("S3 ListBuckets page %d OK", pages)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if pages == 0 {
		t.Fatal("no response from S3")
	}
}
