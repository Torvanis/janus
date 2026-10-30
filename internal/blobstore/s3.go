package blobstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// DefaultRegion is used when an S3 configuration names none; most
// S3-compatible servers accept any region and AWS treats it as the default.
const DefaultRegion = "us-east-1"

// S3Config is everything needed to reach one S3-compatible bucket.
type S3Config struct {
	Endpoint        string
	Bucket          string
	Region          string
	PathStyle       bool
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	// CABundle is extra PEM certificates trusted for the endpoint (a
	// private CA), on top of the system roots.
	CABundle string
}

// Validate reports the first problem with the configuration, worded for an
// administrator.
func (c S3Config) Validate() error {
	u, err := url.Parse(strings.TrimSpace(c.Endpoint))
	switch {
	case strings.TrimSpace(c.Endpoint) == "":
		return errors.New("the S3 endpoint is required")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return errors.New("the S3 endpoint must be an http:// or https:// URL")
	case u.User != nil:
		return errors.New("the S3 endpoint must not contain credentials")
	}
	if err := validateBucket(c.Bucket); err != nil {
		return err
	}
	if strings.TrimSpace(c.AccessKeyID) == "" {
		return errors.New("the S3 access key ID is required")
	}
	if c.SecretAccessKey == "" {
		return errors.New("the S3 secret access key is required")
	}
	if p := strings.Trim(c.Prefix, "/"); p != "" {
		if err := ValidateKey(p); err != nil {
			return fmt.Errorf("the S3 prefix is invalid: %w", err)
		}
	}
	if strings.TrimSpace(c.CABundle) != "" {
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CABundle)) {
			return errors.New("the CA bundle contains no valid PEM certificate")
		}
	}
	return nil
}

func validateBucket(b string) error {
	if len(b) < 3 || len(b) > 63 {
		return errors.New("the S3 bucket name must be 3 to 63 characters")
	}
	for i, r := range b {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || ((r == '-' || r == '.') && i > 0 && i < len(b)-1)
		if !ok {
			return errors.New("the S3 bucket name may contain only lowercase letters, digits, dots and hyphens, and must start and end with a letter or digit")
		}
	}
	return nil
}

// S3 is the S3-compatible backend. The bucket is created on first use when
// it does not exist (an already-owned bucket is fine).
type S3 struct {
	cfg    S3Config
	client *s3.Client
	prefix string

	bucketMu    sync.Mutex
	bucketReady bool
}

// NewS3 builds the backend. It does not contact the server; Check does.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(cfg.CABundle) != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM([]byte(cfg.CABundle))
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	httpClient := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: cfg.PathStyle,
		Credentials:  aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
		HTTPClient:   httpClient,
		// Only send/verify the newer flexible checksums when an operation
		// requires them: several S3-compatible servers reject or mangle
		// the aws-chunked CRC trailers the SDK sends by default.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           3,
	})
	return &S3{cfg: cfg, client: client, prefix: strings.Trim(cfg.Prefix, "/")}, nil
}

// Kind implements Store.
func (s *S3) Kind() string { return KindS3 }

func (s *S3) objectKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return path.Join(s.prefix, key)
}

// EnsureBucket creates the bucket when it is missing. It remembers success,
// so the round trip happens once per backend instance.
func (s *S3) EnsureBucket(ctx context.Context) error {
	s.bucketMu.Lock()
	defer s.bucketMu.Unlock()
	if s.bucketReady {
		return nil
	}
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.cfg.Bucket)})
	if err == nil {
		s.bucketReady = true
		return nil
	}
	if !isNotFound(err) {
		// Forbidden/unreachable: HeadBucket has no body, so its error
		// carries no reason. Leave it to the next request, whose error
		// names the cause (SignatureDoesNotMatch, InvalidAccessKeyId …).
		return nil
	}
	in := &s3.CreateBucketInput{Bucket: aws.String(s.cfg.Bucket)}
	if s.cfg.Region != DefaultRegion {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(s.cfg.Region)}
	}
	if _, err := s.client.CreateBucket(ctx, in); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) {
			return describe("create bucket "+s.cfg.Bucket, err)
		}
	}
	s.bucketReady = true
	return nil
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := s.EnsureBucket(ctx); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(s.objectKey(key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return describe("put object", err)
	}
	return nil
}

// Get implements Store.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.objectKey(key))})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, describe("get object", err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("get object: read body: %w", err)
	}
	return data, nil
}

// Delete implements Store.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.objectKey(key))})
	if err != nil && !isNotFound(err) {
		return describe("delete object", err)
	}
	return nil
}

// Check implements Store: create the bucket if needed, then write, read
// back and delete a probe object.
func (s *S3) Check(ctx context.Context) error {
	if err := s.EnsureBucket(ctx); err != nil {
		return err
	}
	return roundTrip(ctx, s)
}

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	var nsb *types.NoSuchBucket
	if errors.As(err, &nsk) || errors.As(err, &nf) || errors.As(err, &nsb) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket", "404":
			return true
		}
	}
	return false
}

// describe turns an SDK error into one line an administrator can act on,
// keeping the provider's own code and message.
func describe(op string, err error) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		hint := ""
		switch api.ErrorCode() {
		case "SignatureDoesNotMatch":
			hint = " (the secret access key is wrong)"
		case "InvalidAccessKeyId":
			hint = " (the access key ID is not known to the storage service)"
		case "AccessDenied", "Forbidden":
			hint = " (the credentials lack permission for this bucket)"
		case "NoSuchBucket":
			hint = " (the bucket does not exist)"
		}
		msg := api.ErrorMessage()
		if msg == "" || msg == api.ErrorCode() {
			return fmt.Errorf("%s: %s%s", op, api.ErrorCode(), hint)
		}
		return fmt.Errorf("%s: %s: %s%s", op, api.ErrorCode(), msg, hint)
	}
	return fmt.Errorf("%s: %w", op, err)
}
