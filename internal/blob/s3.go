package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is the S3_* configuration. Credentials are optional: unset, the
// usual chain applies (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, the
// shared credentials file, an instance / task / pod role). Endpoint is for
// S3-compatible stores (MinIO, R2, Backblaze, Ceph); empty means AWS.
type S3Config struct {
	Bucket    string
	Endpoint  string // host[:port], with an optional http:// or https:// (default https)
	Region    string
	AccessKey string
	SecretKey string
	Prefix    string // prepended to every key, e.g. "crashcart/"
}

// S3 is a Store on one bucket, through minio-go (path-style addressing for
// non-AWS endpoints is automatic).
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewS3 builds the client. It does not touch the bucket — Ping does.
func NewS3(_ context.Context, c S3Config) (*S3, error) {
	if c.Bucket == "" {
		return nil, errors.New("S3_BUCKET is required")
	}
	endpoint, secure := c.Endpoint, true
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	if s, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint, secure = s, false
	} else if s, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint = s
	}
	var creds *credentials.Credentials
	switch {
	case c.AccessKey != "" && c.SecretKey != "":
		creds = credentials.NewStaticV4(c.AccessKey, c.SecretKey, "")
	case c.AccessKey != "" || c.SecretKey != "":
		return nil, errors.New("S3_ACCESS_KEY and S3_SECRET_KEY must be set together")
	default:
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{}, &credentials.EnvMinio{}, &credentials.FileAWSCredentials{}, &credentials.IAM{},
		})
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: creds, Secure: secure, Region: c.Region})
	if err != nil {
		return nil, fmt.Errorf("S3_ENDPOINT %q: %w", c.Endpoint, err)
	}
	return &S3{client: client, bucket: c.Bucket, prefix: c.Prefix}, nil
}

func (s *S3) key(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	k, err := s.key(key)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, s.bucket, k, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	k, err := s.key(key)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, k, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj) // the request happens on the first read; a missing key surfaces here
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// GetRange reads bytes [off, off+n) of an object — one payload out of a
// pack — as a single ranged request.
func (s *S3) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	k, err := s.key(key)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return []byte{}, nil
	}
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, off+n-1); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, k, opts)
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if int64(len(data)) != n {
		return nil, fmt.Errorf("blob: short range read of %s: %d of %d bytes", key, len(data), n)
	}
	return data, nil
}

// Delete: S3 answers a delete of a missing key with success, so there is
// nothing to translate.
func (s *S3) Delete(ctx context.Context, key string) error {
	k, err := s.key(key)
	if err != nil {
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, k, minio.RemoveObjectOptions{})
}

// Ping checks the bucket is reachable with these credentials, for a clear
// error at startup rather than at the first upload.
func (s *S3) Ping(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("S3 bucket %q: %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("S3 bucket %q does not exist", s.bucket)
	}
	return nil
}

// EnsureBucket creates the bucket when it does not exist. Not called at
// startup (a production bucket is provisioned with its own policy, and the
// credentials may lack CreateBucket); tests use it against MinIO.
func (s *S3) EnsureBucket(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil || ok {
		return err
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}
