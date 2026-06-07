package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3BlobStore is the S3-compatible backend. Works against any
// S3-protocol endpoint: MinIO, R2, B2, AWS S3, Storj, Wasabi.
// Bytes are keyed by their SHA256, same as the disk store; no
// internal sharding since S3-compatible backends don't need
// directory fanout the way local filesystems do.
type S3BlobStore struct {
	client *minio.Client
	bucket string
}

// S3Config bundles the connection inputs. EndpointURL must include the
// scheme (https://… or http://…); minio-go wants just host[:port], so
// we split it ourselves.
type S3Config struct {
	EndpointURL string // e.g. "https://minio.local:9000" or "https://<account>.r2.cloudflarestorage.com"
	Bucket      string
	Region      string // e.g. "us-east-1"; many providers ignore but minio-go wants something
	AccessKey   string
	SecretKey   string
	UseSSL      bool // overridden by the scheme in EndpointURL when present
}

// NewS3BlobStore builds the client and verifies the bucket exists.
// Returns an error early if connectivity / credentials / bucket
// presence look wrong — better to fail at startup than at first Put.
func NewS3BlobStore(cfg S3Config) (*S3BlobStore, error) {
	if cfg.EndpointURL == "" {
		return nil, errors.New("s3: endpoint url required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: access key + secret required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint, useSSL, err := parseEndpoint(cfg.EndpointURL, cfg.UseSSL)
	if err != nil {
		return nil, fmt.Errorf("s3 endpoint: %w", err)
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: useSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	// Bucket-exists probe is the cheapest reachability + auth + perms check.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3 bucket exists check (%s): %w", cfg.Bucket, err)
	}
	if !exists {
		return nil, fmt.Errorf("s3 bucket %q does not exist; create it before starting hostthis", cfg.Bucket)
	}
	return &S3BlobStore{client: client, bucket: cfg.Bucket}, nil
}

// parseEndpoint accepts a full URL like "https://host:port" or a bare
// "host:port" and returns the (host[:port], useSSL) pair minio-go needs.
func parseEndpoint(raw string, fallbackUseSSL bool) (string, bool, error) {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", false, err
		}
		useSSL := u.Scheme == "https"
		return u.Host, useSSL, nil
	}
	return raw, fallbackUseSSL, nil
}

func (s *S3BlobStore) Put(sha string, r io.Reader, size int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Content-addressed: if the object already exists with this key, the
	// bytes are guaranteed identical (sha collision is not a real risk).
	// Skip the upload to save bandwidth, especially helpful for the
	// migrator running idempotently.
	if _, err := s.client.StatObject(ctx, s.bucket, sha, minio.StatObjectOptions{}); err == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	_, err := s.client.PutObject(ctx, s.bucket, sha, r, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", sha, err)
	}
	return nil
}

func (s *S3BlobStore) Get(sha string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	obj, err := s.client.GetObject(ctx, s.bucket, sha, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", sha, err)
	}
	defer obj.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, obj); err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 read %s: %w", sha, err)
	}
	return buf.Bytes(), nil
}

// WalkBlobs iterates every object in the bucket and calls fn(sha).
// Backend-specific note: S3 listings are paginated and (for large
// buckets) can take a while. The sweep runs every 5 minutes and we
// expect < 10k blobs at our scale, so listing is fine.
func (s *S3BlobStore) WalkBlobs(fn func(sha string) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("s3 list: %w", obj.Err)
		}
		if err := fn(obj.Key); err != nil {
			return err
		}
	}
	return nil
}

func (s *S3BlobStore) Remove(sha string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.client.RemoveObject(ctx, s.bucket, sha, minio.RemoveObjectOptions{}); err != nil {
		if isS3NotFound(err) {
			return nil // mirror disk-store's no-op-on-missing semantics
		}
		return fmt.Errorf("s3 remove %s: %w", sha, err)
	}
	return nil
}

func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.StatusCode == 404
	}
	return false
}
