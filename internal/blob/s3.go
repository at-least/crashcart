package blob

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// S3Config is the bucket to use. Endpoint "" means AWS S3 in Region; any
// other endpoint (MinIO, R2, Backblaze, …) is used as given, with or
// without a scheme (https when none).
type S3Config struct {
	Bucket    string
	Endpoint  string
	Region    string // "us-east-1" when empty
	AccessKey string
	SecretKey string
	Prefix    string // key prefix inside the bucket ("" = the bucket root)
}

// S3 is a Store on an S3-compatible bucket (minio-go). Put stores objects
// gzipped (Content-Encoding: gzip; Get decodes); PutRaw / GetRange move
// bytes as they are (the payload packs, which gzip each payload
// themselves so a range is decodable on its own).
type S3 struct {
	cfg    S3Config
	client *minio.Client
}

// NewS3 validates cfg and returns a client (no network call).
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("S3_BUCKET is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3_ACCESS_KEY and S3_SECRET_KEY are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	if cfg.Prefix != "" {
		cfg.Prefix += "/"
	}
	endpoint := strings.TrimSuffix(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	} else if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("S3_ENDPOINT %q: not a URL", cfg.Endpoint)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: u.Scheme == "https",
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("object store: %w", err)
	}
	return &S3{cfg: cfg, client: client}, nil
}

// Bucket is the configured bucket name.
func (s *S3) Bucket() string { return s.cfg.Bucket }

func (s *S3) key(k string) string { return s.cfg.Prefix + k }

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(data)
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, s.cfg.Bucket, s.key(key), bytes.NewReader(buf.Bytes()), int64(buf.Len()),
		minio.PutObjectOptions{ContentType: "application/octet-stream", ContentEncoding: "gzip"})
	return wrap("put", key, err)
}

// PutRaw stores data exactly as given (no Content-Encoding).
func (s *S3) PutRaw(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.cfg.Bucket, s.key(key), bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return wrap("put", key, err)
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	body, err := s.read(ctx, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// Decode what Put wrote, whether or not the transport already did:
	// gzip's magic bytes say.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", key, err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", key, err)
	}
	return out, nil
}

// GetRange reads n bytes at off of a PutRaw object. A range past the end
// or a missing object is ErrNotFound.
func (s *S3) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, off+n-1); err != nil {
		return nil, err
	}
	body, err := s.read(ctx, key, opts)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != n {
		return nil, fmt.Errorf("blob %s: range %d+%d returned %d bytes", key, off, n, len(body))
	}
	return body, nil
}

func (s *S3) read(ctx context.Context, key string, opts minio.GetObjectOptions) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.cfg.Bucket, s.key(key), opts)
	if err != nil {
		return nil, wrap("get", key, err)
	}
	defer obj.Close()
	body, err := io.ReadAll(obj)
	if err != nil {
		return nil, wrap("get", key, err)
	}
	return body, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	err := wrap("delete", key, s.client.RemoveObject(ctx, s.cfg.Bucket, s.key(key), minio.RemoveObjectOptions{}))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// EnsureBucket creates the bucket when it does not exist (a fresh MinIO;
// managed providers usually have it created by hand).
func (s *S3) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("bucket %s: %w", s.cfg.Bucket, err)
	}
	if exists {
		return nil
	}
	err = s.client.MakeBucket(ctx, s.cfg.Bucket, minio.MakeBucketOptions{Region: s.cfg.Region})
	if err != nil {
		code := minio.ToErrorResponse(err).Code
		if code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists" {
			return nil
		}
		return fmt.Errorf("create bucket %s: %w", s.cfg.Bucket, err)
	}
	return nil
}

// LifecycleRule expires the objects under Prefix Days after they were
// written.
type LifecycleRule struct {
	ID     string
	Prefix string
	Days   int
}

// SetLifecycle replaces the bucket's lifecycle configuration with rules
// (CrashCart owns the bucket's lifecycle: use a dedicated bucket or accept
// that other rules are overwritten).
func (s *S3) SetLifecycle(ctx context.Context, rules []LifecycleRule) error {
	cfg := lifecycle.NewConfiguration()
	for _, r := range rules {
		cfg.Rules = append(cfg.Rules, lifecycle.Rule{
			ID:         r.ID,
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: s.cfg.Prefix + r.Prefix},
			Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(r.Days)},
		})
	}
	if err := s.client.SetBucketLifecycle(ctx, s.cfg.Bucket, cfg); err != nil {
		return fmt.Errorf("bucket %s lifecycle: %w", s.cfg.Bucket, err)
	}
	return nil
}

// wrap maps the provider's not-found answers to ErrNotFound.
func wrap(op, key string, err error) error {
	if err == nil {
		return nil
	}
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NotFound", "InvalidRange":
		return ErrNotFound
	}
	return fmt.Errorf("blob %s %s: %w", op, key, err)
}
